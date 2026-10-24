// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Command testlogosmove is the pure-Go program behind
// TestLogosMoveScenarios: it fragments the heap, forces collections, and
// checks that a moving collector keeps every reference intact. It runs the
// same under any collector; LOGOS_EXPECT_MOVE=1 additionally requires that
// objects moved and LOGOS_EXPECT_MOVE=0 that none did.
package main

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"slices"
	"sync"
	"syscall"
	"time"
	"unique"
	"unsafe"
	"weak"
)

//go:linkname heapObjectsCanMove runtime.heapObjectsCanMove
func heapObjectsCanMove() bool

type node struct {
	next *node
	v    int
	pad  [5]uintptr
}

type holder struct {
	child *node
	pad   [100]byte
}

var (
	sink8     **node
	gnode     *node
	gholder   *holder
	resurrect *holder
	expect    = os.Getenv("LOGOS_EXPECT_MOVE")
)

const (
	groupSize = 20000
	keepEvery = 10
	hookAt    = 5
)

func fragment(hook func(last *node)) []*node {
	dense := make([]*node, groupSize)
	for i := range dense {
		dense[i] = &node{v: i}
		sink8 = new(*node)
	}
	for i := range dense {
		if i%5 >= 3 {
			dense[i] = nil
		}
	}
	sparse := make([]*node, 0, groupSize/keepEvery)
	for i := range groupSize {
		n := &node{v: 1000000 + i}
		sink8 = new(*node)
		if i%keepEvery == 0 {
			if len(sparse) > 0 {
				sparse[len(sparse)-1].next = n
			}
			sparse = append(sparse, n)
		}
		if i%keepEvery == hookAt && hook != nil {
			hook(sparse[len(sparse)-1])
		}
	}
	gnode = dense[0]
	return sparse
}

func addrs(ns []*node) []uintptr {
	out := make([]uintptr, len(ns))
	for i, n := range ns {
		out[i] = uintptr(unsafe.Pointer(n))
	}
	return out
}

func countMoved(before []uintptr, ns []*node) int {
	moved := 0
	for i, n := range ns {
		if uintptr(unsafe.Pointer(n)) != before[i] {
			moved++
		}
	}
	return moved
}

func verifyChain(ns []*node) error {
	for i, n := range ns {
		if n.v != 1000000+i*keepEvery {
			return fmt.Errorf("survivor %d holds %d", i, n.v)
		}
		if i+1 < len(ns) && n.next != ns[i+1] {
			return fmt.Errorf("survivor %d: next does not point to survivor %d", i, i+1)
		}
	}
	return nil
}

func requireMoved(moved, total int) error {
	switch expect {
	case "1":
		if moved < total/4 {
			return fmt.Errorf("only %d of %d survivors moved", moved, total)
		}
	case "0":
		if moved != 0 {
			return fmt.Errorf("%d survivors moved without a moving collector", moved)
		}
	}
	return nil
}

func finish(before []uintptr, sparse []*node) error {
	if err := verifyChain(sparse); err != nil {
		return err
	}
	return requireMoved(countMoved(before, sparse), len(sparse))
}

func waitGC(done <-chan struct{}, what string) error {
	deadline := time.After(30 * time.Second)
	for {
		runtime.GC()
		select {
		case <-done:
			return nil
		case <-deadline:
			return fmt.Errorf("%s did not run", what)
		case <-time.After(2 * time.Millisecond):
		}
	}
}

func canMove() error {
	if expect != "" && heapObjectsCanMove() != (expect == "1") {
		return fmt.Errorf("heapObjectsCanMove() = %v, want %v", heapObjectsCanMove(), expect == "1")
	}
	return nil
}

func evacuate() error {
	sparse := fragment(nil)
	local := sparse[3]
	gnode = sparse[1]
	interior := &sparse[2].pad[3]
	ch := make(chan *node, 1)
	ch <- sparse[5]
	mv := map[int]*node{1: sparse[6]}
	var iface any = sparse[7]
	gate, res := make(chan struct{}), make(chan *node)
	go func(p *node) {
		<-gate
		res <- p
	}(sparse[4])
	runtime.Gosched()
	before := addrs(sparse)
	runtime.GC()
	moved := countMoved(before, sparse)
	switch {
	case local != sparse[3]:
		return errors.New("stack slot points to a stale copy")
	case gnode != sparse[1]:
		return errors.New("global points to a stale copy")
	case interior != &sparse[2].pad[3]:
		return errors.New("interior pointer lost its offset")
	case <-ch != sparse[5]:
		return errors.New("channel buffer points to a stale copy")
	case mv[1] != sparse[6] || iface.(*node) != sparse[7]:
		return errors.New("map value or interface points to a stale copy")
	}
	close(gate)
	if <-res != sparse[4] {
		return errors.New("parked goroutine's stack points to a stale copy")
	}
	if err := verifyChain(sparse); err != nil {
		return err
	}
	fmt.Printf("moved=%d/%d ", moved, len(sparse))
	return requireMoved(moved, len(sparse))
}

func pinner() error {
	sparse := fragment(nil)
	var pin runtime.Pinner
	pin.Pin(sparse[8])
	before := addrs(sparse)
	runtime.GC()
	defer pin.Unpin()
	if uintptr(unsafe.Pointer(sparse[8])) != before[8] {
		return errors.New("pinned object moved")
	}
	return finish(before, sparse)
}

func mapKey() error {
	sparse := fragment(nil)
	byPtr := map[*node]int{}
	byIface := map[any]int{}
	var bySync sync.Map
	keys := []int{10, 20, 30, 40}
	for _, k := range keys {
		byPtr[sparse[k]] = k
		byIface[sparse[k+1]] = k
		bySync.Store(sparse[k+2], k)
	}
	before := addrs(sparse)
	runtime.GC()
	for _, k := range keys {
		if v, _ := bySync.Load(sparse[k+2]); byPtr[sparse[k]] != k || byIface[sparse[k+1]] != k || v != k {
			return fmt.Errorf("pointer-keyed lookup %d failed after collection", k)
		}
		for d := range 3 {
			if uintptr(unsafe.Pointer(sparse[k+d])) != before[k+d] {
				return fmt.Errorf("map key %d moved", k+d)
			}
		}
	}
	return finish(before, sparse)
}

func conservativeFrame() error {
	sparse := fragment(nil)
	var fds [2]int
	if err := syscall.Pipe(fds[:]); err != nil {
		return err
	}
	held := make(chan uintptr)
	go func(p *node) {
		held <- uintptr(unsafe.Pointer(p))
		var b [1]byte
		syscall.Read(fds[0], b[:])
		runtime.KeepAlive(p)
	}(sparse[9])
	at := <-held
	time.Sleep(100 * time.Millisecond)
	before := addrs(sparse)
	runtime.GC()
	if uintptr(unsafe.Pointer(sparse[9])) != at {
		return errors.New("object referenced from a frame blocked in a system call moved")
	}
	syscall.Write(fds[1], []byte{1})
	return finish(before, sparse)
}

func weakUnique() error {
	sparse := fragment(nil)
	wp := weak.Make(sparse[11])
	h := unique.Make([2]string{"logos", "move"})
	before := addrs(sparse)
	runtime.GC()
	if wp.Value() != sparse[11] {
		return errors.New("weak pointer lost its target")
	}
	if h2 := unique.Make([2]string{"logos", "move"}); h2 != h || h.Value()[1] != "move" {
		return errors.New("unique handle changed across a collection")
	}
	return finish(before, sparse)
}

func finalizerRegistration() error {
	sparse := fragment(nil)
	first, second := make(chan struct{}), make(chan struct{})
	func() {
		h := &holder{child: sparse[12]}
		runtime.SetFinalizer(h, func(*holder) { close(first) })
		runtime.SetFinalizer(h, nil)
		runtime.SetFinalizer(h, func(o *holder) {
			if o.child != sparse[12] {
				panic("finalizer sees a stale child")
			}
			close(second)
		})
	}()
	before := addrs(sparse)
	if err := waitGC(second, "re-registered finalizer"); err != nil {
		return err
	}
	select {
	case <-first:
		return errors.New("removed finalizer ran")
	default:
	}
	return finish(before, sparse)
}

func finalizerQueued() error {
	sparse := fragment(nil)
	started, gate := make(chan struct{}), make(chan struct{})
	ran := make(chan int, 2)
	for i := range 2 {
		func() {
			h := &holder{child: sparse[13+i]}
			c := sparse[13+i]
			runtime.SetFinalizer(h, func(o *holder) {
				if i == 0 {
					close(started)
					<-gate
				}
				if o.child != c || c != sparse[13+i] {
					panic("finalizer sees a stale child")
				}
				ran <- i
			})
		}()
	}
	before := addrs(sparse)
	if err := waitGC(started, "first finalizer"); err != nil {
		return err
	}
	runtime.GC()
	runtime.GC()
	close(gate)
	deadline := time.After(30 * time.Second)
	for got := 0; got < 2; {
		select {
		case <-ran:
			got++
		case <-deadline:
			return errors.New("queued finalizer did not run")
		case <-time.After(2 * time.Millisecond):
			runtime.GC()
		}
	}
	return finish(before, sparse)
}

func finalizerResurrection() error {
	sparse := fragment(nil)
	done := make(chan struct{})
	func() {
		h := &holder{child: sparse[15]}
		runtime.SetFinalizer(h, func(o *holder) {
			resurrect = o
			close(done)
		})
	}()
	before := addrs(sparse)
	if err := waitGC(done, "resurrecting finalizer"); err != nil {
		return err
	}
	runtime.GC()
	runtime.GC()
	if resurrect == nil || resurrect.child != sparse[15] {
		return errors.New("resurrected object lost its child")
	}
	return finish(before, sparse)
}

func finalizerReclaims() error {
	sparse := fragment(nil)
	done := make(chan struct{})
	var siblings []weak.Pointer[holder]
	func() {
		for i := range 20 {
			h := &holder{}
			if i == 10 {
				runtime.SetFinalizer(h, func(*holder) { close(done) })
				continue
			}
			siblings = append(siblings, weak.Make(h))
		}
	}()
	if err := waitGC(done, "finalizer on a shared span"); err != nil {
		return err
	}
	runtime.GC()
	for i, s := range siblings {
		if s.Value() != nil {
			return fmt.Errorf("dead sibling %d survived in a span with a finalizer", i)
		}
	}
	return verifyChain(sparse)
}

const (
	cleanups = 200
	// regionStride is the number of 64-byte nodes the cleanup region holds
	// per cleanup: one argument and regionStride-1 others.
	regionStride = 20
	// blockBytes is the size of an immix block (M19-S8). A block with more
	// than blockForeign 64-byte region objects has no page left for a span
	// of anything else: three 8 KiB pages hold at most that many.
	blockBytes   = 32 << 10
	blockForeign = 3 * 8192 / 64
)

// cleanupMoved attaches cleanups whose arguments and closures sit in a
// region of 64-byte objects, queues them behind a blocked cleanup, and lets
// the next collection move them while queued.
//
// Which block the collector evacuates is decided by layout, so the scenario
// fixes the layout instead of counting on it. A block that holds more than
// blockForeign region objects is made of region spans only: no pinned,
// flagged, or foreign span can share it. During the collection that queues
// the cleanups the region is dense, since its other nodes are still
// reachable, and such a block stays. They are dropped before the next
// collection, which finds such a block fragmented and must move every
// argument and closure in it. Free slots for the moved objects are
// allocated before the region and freed only for that collection, twice as
// many as the region needs; filler of whole-page objects first takes the
// free pages, so that the slots sit in dense blocks of their own and not in
// fragmented ones that would evacuate as well.
func cleanupMoved() error {
	gate, blocked := make(chan struct{}), make(chan struct{})
	blocker := new(holder)
	runtime.AddCleanup(blocker, func(int) {
		close(blocked)
		<-gate
	}, 0)
	blocker = nil
	got := make(chan *node, cleanups)
	owners := make([]*holder, cleanups)
	for i := range owners {
		owners[i] = new(holder)
	}
	args := make([]*node, 0, cleanups)
	kept := make([]func(*node), 0, cleanups)
	argAt := make([]uintptr, 0, cleanups)
	clAt := make([]uintptr, 0, cleanups)
	others := make([]*node, 0, cleanups*(regionStride-1))
	region := make([]uintptr, 0, cleanups*(regionStride+1))
	if err := waitGC(blocked, "blocking cleanup"); err != nil {
		return err
	}

	filler := make([]*[8192]byte, 512)
	for i := range filler {
		filler[i] = new([8192]byte)
	}
	dest := make([]*node, 3*4*cleanups)
	for i := range dest {
		dest[i] = new(node)
	}

	for i := range cleanups {
		for range regionStride / 2 {
			others = append(others, new(node))
		}
		a := &node{v: 1000000 + i}
		for range regionStride/2 - 1 {
			others = append(others, new(node))
		}
		// Values derived from i, so that the closure captures all five and
		// fills a 64-byte object like the nodes around it.
		a0, a1, a2, a3, a4 := i, i+1, i+2, i+3, i+4
		cl := func(arg *node) {
			if a1 != a0+1 || a2 != a0+2 || a3 != a0+3 || a4 != a0+4 || arg != a {
				panic("cleanup closure lost a captured value")
			}
			got <- arg
		}
		runtime.AddCleanup(owners[i], cl, a)
		args, kept = append(args, a), append(kept, cl)
		argAt = append(argAt, uintptr(unsafe.Pointer(a)))
		clAt = append(clAt, *(*uintptr)(unsafe.Pointer(&cl)))
	}
	for _, n := range others {
		region = append(region, uintptr(unsafe.Pointer(n))/blockBytes)
	}
	for i := range args {
		region = append(region, argAt[i]/blockBytes, clAt[i]/blockBytes)
	}
	slices.Sort(region)
	whole := func(p uintptr) bool {
		b := p / blockBytes
		i, _ := slices.BinarySearch(region, b)
		return i+blockForeign < len(region) && region[i+blockForeign] == b
	}

	clear(owners)
	runtime.GC()
	for i := range args {
		if whole(argAt[i]) && uintptr(unsafe.Pointer(args[i])) != argAt[i] || whole(clAt[i]) && *(*uintptr)(unsafe.Pointer(&kept[i])) != clAt[i] {
			return fmt.Errorf("cleanup %d of the dense region moved before it was queued", i)
		}
	}
	clear(others)
	for i := range dest {
		if i%3 == 2 {
			dest[i] = nil
		}
	}
	runtime.GC()

	var argsMoved, closuresMoved, argsInside, closuresInside int
	for i := range args {
		argMoved := uintptr(unsafe.Pointer(args[i])) != argAt[i]
		clMoved := *(*uintptr)(unsafe.Pointer(&kept[i])) != clAt[i]
		if argMoved {
			argsMoved++
		}
		if clMoved {
			closuresMoved++
		}
		if whole(argAt[i]) {
			argsInside++
			if expect == "1" && !argMoved {
				return fmt.Errorf("argument %d in region block %#x did not move", i, argAt[i]&^(blockBytes-1))
			}
		}
		if whole(clAt[i]) {
			closuresInside++
			if expect == "1" && !clMoved {
				return fmt.Errorf("closure %d in region block %#x did not move", i, clAt[i]&^(blockBytes-1))
			}
		}
	}
	if argsInside < cleanups/2 || closuresInside < cleanups/2 {
		return fmt.Errorf("region layout: only %d arguments and %d closures of %d in region blocks", argsInside, closuresInside, cleanups)
	}
	if err := requireMoved(argsMoved, cleanups); err != nil {
		return err
	}
	if err := requireMoved(closuresMoved, cleanups); err != nil {
		return err
	}

	close(gate)
	deadline := time.After(30 * time.Second)
	seen := make([]bool, cleanups)
	for ran := 0; ran < cleanups; {
		select {
		case a := <-got:
			k := a.v - 1000000
			if k < 0 || k >= cleanups || args[k] != a || seen[k] {
				return errors.New("cleanup received a stale argument")
			}
			seen[k] = true
			ran++
		case <-deadline:
			return fmt.Errorf("only %d of %d queued cleanups ran", ran, cleanups)
		case <-time.After(2 * time.Millisecond):
			runtime.GC()
		}
	}
	runtime.KeepAlive(dest)
	runtime.KeepAlive(filler)
	fmt.Printf("arguments moved=%d/%d closures moved=%d/%d (in region blocks %d/%d) ", argsMoved, cleanups, closuresMoved, cleanups, argsInside, closuresInside)
	return nil
}

func cleanupStop() error {
	sparse := fragment(nil)
	ran := make(chan struct{}, 1)
	func() {
		h := &holder{child: sparse[17]}
		c := runtime.AddCleanup(h, func(*node) { ran <- struct{}{} }, sparse[17])
		c.Stop()
	}()
	for range 5 {
		runtime.GC()
	}
	select {
	case <-ran:
		return errors.New("stopped cleanup ran")
	default:
	}
	return verifyChain(sparse)
}

var scenarios = map[string]func() error{
	"canmove":      canMove,
	"evacuate":     evacuate,
	"pinner":       pinner,
	"mapkey":       mapKey,
	"syscall":      conservativeFrame,
	"weakunique":   weakUnique,
	"finreg":       finalizerRegistration,
	"finqueued":    finalizerQueued,
	"finresurrect": finalizerResurrection,
	"finreclaim":   finalizerReclaims,
	"cleanupmoved": cleanupMoved,
	"cleanupstop":  cleanupStop,
}

func main() {
	run := scenarios[os.Args[len(os.Args)-1]]
	if run == nil {
		fmt.Fprintln(os.Stderr, "usage: testlogosmove scenario")
		os.Exit(2)
	}
	debug.SetGCPercent(-1)
	if err := run(); err != nil {
		fmt.Println("FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("OK")
}
