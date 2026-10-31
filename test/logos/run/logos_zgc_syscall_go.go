// run -goexperiment logosgcmoving -gcflags=all=-lb "-ldflags=-X runtime.logosGCMode=zgc"

//go:build amd64 || arm64

// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Plain-Go mirror of test/logos/compile/logos_zgc_syscall.lgo for the raw
// goroutine and defer forms loGo/s cannot spell: loGo/s admits only go
// group children, but ordinary Go packages call the syscall entries
// straight from go and defer statements, where the compiler normalizes the
// call into a wrapper closure that captures the unsafe.Pointer operand.
//
// Each round fragments the heap so the payload survives in a mostly dead
// page, starts a collecting goroutine, and blocks a direct syscall readv
// whose iovec array is the only path to the payload: no word of the
// blocked goroutine's frames names it, so selection-time conservative
// scanning of a syscall-blocked stack cannot pin it (a single-pointer read
// would be pinned that way, so it is a control, not a red). Without a
// complete admission closure an evacuation moves the payload while the
// kernel still holds the address captured at syscall entry, so the canary
// check fails; with admission the payload is pinned for the call. The zgc
// mode comes from the directive; the attack must never run under a
// nonmoving collector as a substitute.
//
// Forms: direct go statement, go closure, direct defer, and a go closure
// calling a reassigned function value. Direct go statements offer no hook
// for the reader's first attempt, so that one form settles for a fixed
// margin after the signal instead; every other form signals from inside
// the goroutine just before the trap. Silent on success.

package main

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

const (
	groupSize    = 20000
	keepEvery    = 10
	slotBytes    = 64
	inBlockCycle = 10
	joinDeadline = 10 * time.Second
)

var (
	sinkSlot  *slot
	sinkBytes []byte
)

// slot is a pointer-bearing heap object whose interior byte array is the
// kernel target of the scan-object rounds.
type slot struct {
	next *slot
	data [56]byte
}

// storm carries the reader's first-attempt signal to the collecting
// goroutine.
type storm struct {
	attempts atomic.Uint64
}

func openPipe() (rfd, wfd int, err error) {
	var p [2]int
	if err = syscall.Pipe(p[:]); err != nil {
		return 0, 0, err
	}
	return p[0], p[1], nil
}

func writeAll(fd int, data []byte) error {
	sent := 0
	for sent < len(data) {
		n, err := syscall.Write(fd, data[sent:])
		if err != nil {
			return err
		}
		if n == 0 {
			return errors.New("write made no progress")
		}
		sent += n
	}
	return nil
}

func canaryByte(i int) byte {
	return byte(i%250) + 1
}

func canary(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = canaryByte(i)
	}
	return b
}

func compareCanary(buf []byte) error {
	for i := range buf {
		if buf[i] != canaryByte(i) {
			return fmt.Errorf("corruption at byte %d: got %d want %d", i, buf[i], canaryByte(i))
		}
	}
	return nil
}

// syscallFor returns a reassigned function value: the callee is not
// static at the call site that uses it. A nonempty buffer selects the
// trapping entry, the only one that lets a blocked read overlap a
// collection.
func syscallFor(buf []byte) func(trap, a1, a2, a3 uintptr) (uintptr, uintptr, syscall.Errno) {
	f := syscall.RawSyscall
	if len(buf) > 0 {
		f = syscall.Syscall
	}
	return f
}

func fragmentSlots() []*slot {
	dense := make([]*slot, groupSize)
	for i := range dense {
		dense[i] = &slot{}
	}
	for i := range dense {
		if i%5 >= 3 {
			dense[i] = nil
		}
	}
	sparse := make([]*slot, 0, groupSize/keepEvery)
	for i := range groupSize {
		s := &slot{}
		if i%keepEvery == 0 {
			sparse = append(sparse, s)
		}
	}
	sinkSlot = dense[0]
	return sparse
}

func fragmentBytes() [][]byte {
	dense := make([][]byte, groupSize)
	for i := range dense {
		dense[i] = make([]byte, slotBytes)
	}
	for i := range dense {
		if i%5 >= 3 {
			dense[i] = nil
		}
	}
	sparse := make([][]byte, 0, groupSize/keepEvery)
	for i := range groupSize {
		b := make([]byte, slotBytes)
		if i%keepEvery == 0 {
			sparse = append(sparse, b)
		}
	}
	sinkBytes = dense[0]
	return sparse
}

// targetBuf returns a kernel buffer that survives in a fragmented page,
// the shape a relocating collector evacuates, plus the holder that keeps
// its neighbors alive. Kind 0 is the interior of a pointer-bearing object;
// kind 1 is a pointer-free backing.
func targetBuf(kind int) ([]byte, any) {
	if kind == 0 {
		sparse := fragmentSlots()
		t := sparse[len(sparse)/2]
		return t.data[:], sparse
	}
	sparse := fragmentBytes()
	return sparse[len(sparse)/2], sparse
}

// form starts one blocking readv of the iovec through one call shape in a
// goroutine of its own, so the only frames that can be scanned
// conservatively hold the iovec array pointer and never the payload. Every
// form signals s.attempts before the goroutine traps.
type form struct {
	name   string
	settle time.Duration
	run    func(s *storm, rfd int, iov []syscall.Iovec)
}

var forms = []form{
	{"go direct", 50 * time.Millisecond, formGoDirect},
	{"go closure", 0, formGoClosure},
	{"defer direct", 0, formDefer},
	{"go function value", 0, formFuncValue},
}

func formGoDirect(s *storm, rfd int, iov []syscall.Iovec) {
	s.attempts.Add(1)
	go syscall.Syscall(syscall.SYS_READV, uintptr(rfd), uintptr(unsafe.Pointer(&iov[0])), uintptr(len(iov)))
}

func formGoClosure(s *storm, rfd int, iov []syscall.Iovec) {
	go func() {
		s.attempts.Add(1)
		syscall.Syscall(syscall.SYS_READV, uintptr(rfd), uintptr(unsafe.Pointer(&iov[0])), uintptr(len(iov)))
	}()
}

// formDefer runs the deferred call at the return of a goroutine of its own,
// so the blocked stack holds only deferRead's frame.
func formDefer(s *storm, rfd int, iov []syscall.Iovec) {
	go deferRead(s, rfd, iov)
}

func deferRead(s *storm, rfd int, iov []syscall.Iovec) {
	p := unsafe.Pointer(&iov[0])
	s.attempts.Add(1)
	defer syscall.Syscall(syscall.SYS_READV, uintptr(rfd), uintptr(p), uintptr(len(iov)))
}

func formFuncValue(s *storm, rfd int, iov []syscall.Iovec) {
	f := syscallFor(make([]byte, len(iov)))
	go func() {
		s.attempts.Add(1)
		f(syscall.SYS_READV, uintptr(rfd), uintptr(unsafe.Pointer(&iov[0])), uintptr(len(iov)))
	}()
}

// stormGC waits for the reader's first attempt, lets the settle margin
// pass, runs a fixed count of full collections, then writes the canary.
// The deferred close of the write end wakes the reader with end of file
// if anything fails before the write.
func stormGC(s *storm, wfd, size int, settle time.Duration) error {
	defer syscall.Close(wfd)
	for s.attempts.Load() == 0 {
		runtime.Gosched()
	}
	time.Sleep(settle)
	for range inBlockCycle {
		runtime.GC()
	}
	return writeAll(wfd, canary(size))
}

// joinGoroutines waits, bounded, for the goroutine count to fall back to
// want: the reader exits once its syscall returns.
func joinGoroutines(want int) error {
	start := time.Now()
	for runtime.NumGoroutine() > want {
		if time.Since(start) > joinDeadline {
			return errors.New("reader goroutine did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	return nil
}

func round(f form, kind int) error {
	rfd, wfd, err := openPipe()
	if err != nil {
		return err
	}
	buf, keep := targetBuf(kind)
	iov := []syscall.Iovec{{Base: &buf[0]}}
	iov[0].SetLen(len(buf))
	before := runtime.NumGoroutine()
	s := &storm{}
	gcDone := make(chan error, 1)
	go func() { gcDone <- stormGC(s, wfd, len(buf), f.settle) }()
	f.run(s, rfd, iov)
	gcErr := <-gcDone
	joinErr := joinGoroutines(before)
	runtime.KeepAlive(keep)
	syscall.Close(rfd)
	return errors.Join(gcErr, joinErr, compareCanary(buf))
}

func main() {
	failed := false
	for _, f := range forms {
		for kind := range 2 {
			if err := round(f, kind); err != nil {
				fmt.Printf("FAIL %s kind %d: %v\n", f.name, kind, err)
				failed = true
			}
		}
	}
	if failed {
		os.Exit(1)
	}
}
