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
	"sync/atomic"
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

// blob has no pointers: an interface holding one keeps it in a box that no
// pointer map describes, and a collector that moves the box must still leave
// the interface naming it.
type blob [7]uint64

var sinkBox any

// fragmentBoxes is fragment for boxed values: a group of boxes that dies, and
// a group of which 1 in keepEvery survives, so that the survivors' blocks are
// fragmented and the dead group leaves free slots to move them to.
func fragmentBoxes() []any {
	dense := make([]any, groupSize)
	for i := range dense {
		dense[i] = blob{uint64(i)}
	}
	sparse := make([]any, 0, groupSize/keepEvery)
	for i := range groupSize {
		b := any(blob{1000000 + uint64(i)})
		if i%keepEvery == 0 {
			sparse = append(sparse, b)
		}
	}
	sinkBox = dense[0]
	return sparse
}

// boxWord returns the data word of v: the address of the box that holds a
// value of a type that is not pointer-shaped.
func boxWord(v any) uintptr { return (*[2]uintptr)(unsafe.Pointer(&v))[1] }

func boxWords(vs []any) []uintptr {
	out := make([]uintptr, len(vs))
	for i, v := range vs {
		out[i] = boxWord(v)
	}
	return out
}

func boxesMoved(before []uintptr, vs []any) int {
	moved := 0
	for i, v := range vs {
		if boxWord(v) != before[i] {
			moved++
		}
	}
	return moved
}

// target maps the i-th target of a scenario to a survivor index. The targets
// are spread over the survivors, so that they do not share a span and one
// pinned or cached span cannot keep all of them in place.
func target(i int) int { return 100 + i*37 }

// nodesMoved counts the first n targets that changed address since before.
func nodesMoved(before []uintptr, sparse []*node, n int) int {
	moved := 0
	for i := range n {
		if uintptr(unsafe.Pointer(sparse[target(i)])) != before[target(i)] {
			moved++
		}
	}
	return moved
}

// boxesMovedAt counts the first n targets whose box changed address.
func boxesMovedAt(before []uintptr, boxes []any, n int) int {
	moved := 0
	for i := range n {
		if boxWord(boxes[target(i)]) != before[target(i)] {
			moved++
		}
	}
	return moved
}

// requireSomeMoved requires that a collector that moves objects moved one of
// the targets a scenario holds through the root under test, and that one that
// does not move any left them alone. Without it the scenario could pass
// having exercised nothing.
func requireSomeMoved(moved int) error {
	switch {
	case expect == "1" && moved == 0:
		return errors.New("no target moved: the collection did not exercise the root under test")
	case expect == "0" && moved != 0:
		return fmt.Errorf("%d targets moved without a moving collector", moved)
	}
	return nil
}

var (
	sinkJunk    []*node
	sinkJunkBox []any
)

// churn reuses the slots a collection vacated. It allocates and keeps objects
// of the survivors' size classes, so that a pointer left to a vacated slot
// reads another object's bytes: the collector does not clear what it moves.
func churn() {
	sinkJunk = make([]*node, 0, 2*groupSize)
	sinkJunkBox = make([]any, 0, 2*groupSize)
	for i := range 2 * groupSize {
		sinkJunk = append(sinkJunk, &node{v: -1})
		sinkJunkBox = append(sinkJunkBox, blob{^uint64(i)})
	}
}

// deferHeld parks a goroutine in a frame that holds heap-allocated defer
// records, collects, and lets the deferred calls run: each must still see the
// objects its closure captured, at their current addresses.
func deferHeld() error {
	const defers = 24
	sparse := fragment(nil)
	before := addrs(sparse)
	parked, gate := make(chan struct{}), make(chan struct{})
	res := make(chan error, 1)
	go func() {
		var bad error
		func() {
			// The defers are in a loop on purpose: only a loop defer takes
			// the heap-allocated record path (deferproc), and the records
			// the collector must keep reachable and current are those. A
			// per-iteration wrapper would make each one a stack record.
			for i := range defers {
				p, q := sparse[target(i)], &sparse[target(i+defers)].pad[3]
				defer func() {
					switch {
					case p != sparse[target(i)]:
						bad = fmt.Errorf("deferred call %d: captured pointer is a stale copy", i)
					case q != &sparse[target(i+defers)].pad[3]:
						bad = fmt.Errorf("deferred call %d: captured interior pointer lost its offset", i)
					case p.v != 1000000+target(i)*keepEvery:
						bad = fmt.Errorf("deferred call %d: captured object holds %d", i, p.v)
					}
				}()
			}
			parked <- struct{}{}
			<-gate
		}()
		res <- bad
	}()
	<-parked
	runtime.GC()
	churn()
	close(gate)
	if err := <-res; err != nil {
		return err
	}
	if err := requireSomeMoved(nodesMoved(before, sparse, 2*defers)); err != nil {
		return err
	}
	return finish(before, sparse)
}

// heldPanic is a goroutine parked in a deferred call while its panic is in
// flight, so that a collection runs with the panic value held by the
// goroutine's panic record.
type heldPanic struct {
	gate chan struct{}
	out  chan any
}

func holdPanic(v any) *heldPanic {
	h := &heldPanic{gate: make(chan struct{}), out: make(chan any, 1)}
	parked := make(chan struct{})
	go func() {
		defer func() {
			parked <- struct{}{}
			<-h.gate
			h.out <- recover()
		}()
		panic(v)
	}()
	<-parked
	return h
}

// release lets the deferred call recover and returns the recovered value.
func (h *heldPanic) release() any {
	close(h.gate)
	return <-h.out
}

// panicHeld collects while one panic carries a pointer-shaped value and
// another a boxed pointer-free one, and requires recover to return the same
// pointer and the same box, intact.
func panicHeld() error {
	const panics = 12
	sparse := fragment(nil)
	boxes := fragmentBoxes()
	nodeAt, boxAt := addrs(sparse), boxWords(boxes)
	byPtr, byBox := make([]*heldPanic, panics), make([]*heldPanic, panics)
	for i := range panics {
		byPtr[i], byBox[i] = holdPanic(sparse[target(i)]), holdPanic(boxes[target(i)])
	}
	runtime.GC()
	churn()
	for i := range panics {
		k := target(i)
		gotPtr, gotBox := byPtr[i].release(), byBox[i].release()
		if n, ok := gotPtr.(*node); !ok || n != sparse[k] {
			return fmt.Errorf("recovered pointer panic value %d is a stale copy", i)
		}
		if b, ok := gotBox.(blob); !ok || b[0] != 1000000+uint64(k*keepEvery) {
			return fmt.Errorf("recovered boxed panic value %d lost its contents", i)
		}
		if boxWord(gotBox) != boxWord(boxes[k]) {
			return fmt.Errorf("recovered boxed panic value %d names a stale box", i)
		}
	}
	for i, b := range boxes {
		if v, ok := b.(blob); !ok || v[0] != 1000000+uint64(i*keepEvery) {
			return fmt.Errorf("survivor box %d lost its contents", i)
		}
	}
	fmt.Printf("boxes moved=%d/%d ", boxesMoved(boxAt, boxes), len(boxes))
	if err := requireSomeMoved(nodesMoved(nodeAt, sparse, panics)); err != nil {
		return err
	}
	if err := requireSomeMoved(boxesMovedAt(boxAt, boxes, panics)); err != nil {
		return err
	}
	return finish(nodeAt, sparse)
}

// contextHeld runs goroutines that call closures through func values without
// blocking, and collects while they do. A goroutine stopped at a closure's
// prologue keeps the closure in its saved scheduling context; each closure
// must still see its captured object at the current address.
func contextHeld() error {
	sparse := fragment(nil)
	before := addrs(sparse)
	const spinners = 8
	var (
		stop atomic.Bool
		wg   sync.WaitGroup
	)
	bad := make(chan error, spinners)
	for k := range spinners {
		p := sparse[target(k)]
		// Called through a func value, so the call is not inlined. The call
		// to same keeps the closure from being a leaf: a leaf with a small
		// frame gets no stack check, and a goroutine that never reaches one
		// cannot be stopped when asynchronous preemption is off.
		checks := []func() bool{func() bool {
			return same(p, sparse[target(k)]) && p.v == 1000000+target(k)*keepEvery
		}}
		wg.Go(func() {
			for !stop.Load() {
				if !checks[0]() {
					bad <- fmt.Errorf("spinner %d: captured object is a stale copy", k)
					return
				}
			}
		})
	}
	time.Sleep(20 * time.Millisecond)
	for range 3 {
		runtime.GC()
	}
	churn()
	time.Sleep(20 * time.Millisecond)
	stop.Store(true)
	wg.Wait()
	select {
	case err := <-bad:
		return err
	default:
	}
	if err := requireSomeMoved(nodesMoved(before, sparse, spinners)); err != nil {
		return err
	}
	return finish(before, sparse)
}

var sinkStale *node

// same reports whether a and b are the same object. It is not inlined, and
// its failure path is a call that is not inlined either, so same is not a
// leaf and begins with a stack check.
//
//go:noinline
func same(a, b *node) bool {
	if a != b {
		stale(a)
		return false
	}
	return true
}

//go:noinline
func stale(n *node) { sinkStale = n }

// rootsPreserved verifies that a relocation cycle keeps every root kind
// intact: a heap struct with pointer fields, a stack-allocated pointee held
// across the cycle, a package-level global, a channel whose buffer holds a
// pointer, a map with pointer values, and an interface holding a pointer
// type. Each root names a distinct fragmented survivor (with aliasing between
// the heap struct, the stack roots, and the map), so that under immix at
// least one target moves; after one GC-driven relocation plus churn, every
// root must still name the same survivor with the same contents, and the
// channel receive, map lookups, and interface assertion must succeed.
func rootsPreserved() error {
	sparse := fragment(nil)
	const nroots = 6
	i0, i1 := target(0), target(1)
	i2, i3 := target(2), target(3)
	i4, i5 := target(4), target(5)
	type pair struct {
		first  *node
		second *node
	}
	heapStruct := &pair{first: sparse[i0], second: sparse[i1]}
	// Publish through the existing heap-visible global so escape analysis
	// places the pair on the heap: a purely local &pair would stay on the
	// stack and would not exercise a heap-to-heap root.
	sinkBox = heapStruct
	stackVal := node{v: 424242, next: sparse[i1]}
	stackPtr := &stackVal
	stackRoot := sparse[i1]
	gnode = sparse[i2]
	ch := make(chan *node, 1)
	ch <- sparse[i3]
	m := map[int]*node{1: sparse[i4], 2: sparse[i0]}
	var iface any = sparse[i5]
	before := addrs(sparse)
	runtime.GC()
	churn()
	heapStruct = sinkBox.(*pair)
	if heapStruct.first != sparse[i0] {
		return errors.New("heap struct first field points to a stale copy")
	}
	if heapStruct.second != sparse[i1] {
		return errors.New("heap struct second field points to a stale copy")
	}
	if heapStruct.first.v != 1000000+i0*keepEvery || heapStruct.second.v != 1000000+i1*keepEvery {
		return errors.New("heap struct points to objects with wrong contents")
	}
	if stackRoot != sparse[i1] {
		return errors.New("stack slot points to a stale copy")
	}
	if stackPtr.next != sparse[i1] {
		return errors.New("stack-allocated pointee's pointer field points to a stale copy")
	}
	if stackPtr.v != 424242 {
		return errors.New("stack-allocated pointee lost its contents")
	}
	if stackPtr.next.v != 1000000+i1*keepEvery {
		return errors.New("object named from the stack holds wrong contents")
	}
	if gnode != sparse[i2] {
		return errors.New("global points to a stale copy")
	}
	if gnode.v != 1000000+i2*keepEvery {
		return errors.New("object named from a global holds wrong contents")
	}
	got := <-ch
	if got != sparse[i3] {
		return errors.New("channel buffer points to a stale copy")
	}
	if got.v != 1000000+i3*keepEvery {
		return errors.New("object received from channel holds wrong contents")
	}
	if m[1] != sparse[i4] || m[2] != sparse[i0] {
		return errors.New("map value points to a stale copy")
	}
	if _, ok := m[1]; !ok {
		return errors.New("map lookup failed after collection")
	}
	if m[1].v != 1000000+i4*keepEvery || m[2].v != 1000000+i0*keepEvery {
		return errors.New("object named from map holds wrong contents")
	}
	inode, ok := iface.(*node)
	if !ok || inode != sparse[i5] {
		return errors.New("interface points to a stale copy")
	}
	if inode.v != 1000000+i5*keepEvery {
		return errors.New("object named from interface holds wrong contents")
	}
	moved := countMoved(before, sparse)
	fmt.Printf("moved=%d/%d heap=%d,%d stack=%d,%d global=%d chan=%d map=%d,%d iface=%d ", moved, len(sparse), heapStruct.first.v, heapStruct.second.v, stackPtr.v, stackPtr.next.v, gnode.v, got.v, m[1].v, m[2].v, inode.v)
	if err := requireSomeMoved(nodesMoved(before, sparse, nroots)); err != nil {
		return err
	}
	return finish(before, sparse)
}

// interiorHeld holds interior pointers as map keys, cleanup arguments, and
// finalizer targets across a relocation cycle. Map keys hash by address, so
// the collector pins their spans (maps_logosPinMapKey) and a lookup with the
// current interior address must keep finding the entry. Cleanup arguments
// and finalizer targets move with their objects, so the queued cleanup and
// the finalizer must observe the current interior address with its marker
// intact: a stale copy would name a vacated slot that churn reuses.
func interiorHeld() error {
	sparse := fragment(nil)
	iMap0, iMap1 := target(0), target(1)
	iCl, iFin := target(2), target(3)
	const (
		mk0 = uintptr(0x1c10c0de)
		mk1 = uintptr(0x1c10c1de)
		mk2 = uintptr(0x1c10c2de)
		mk3 = uintptr(0x1c10c3de)
	)
	sparse[iMap0].pad[3] = mk0
	sparse[iMap1].pad[3] = mk1
	sparse[iCl].pad[1] = mk2
	sparse[iFin].pad[2] = mk3
	m := map[*uintptr]int{
		&sparse[iMap0].pad[3]: iMap0,
		&sparse[iMap1].pad[3]: iMap1,
	}
	clGot := make(chan *uintptr, 1)
	func() {
		o := &holder{child: sparse[iCl]}
		runtime.AddCleanup(o, func(arg *uintptr) {
			if arg == nil || *arg != mk2 {
				panic("cleanup sees a stale interior argument")
			}
			clGot <- arg
		}, &sparse[iCl].pad[1])
	}()
	finDone := make(chan struct{})
	func() {
		h := &holder{child: sparse[iFin]}
		runtime.SetFinalizer(h, func(o *holder) {
			if o.child != sparse[iFin] {
				panic("finalizer sees a stale child")
			}
			if in := &o.child.pad[2]; in != &sparse[iFin].pad[2] || *in != mk3 {
				panic("finalizer sees a stale interior address")
			}
			close(finDone)
		})
	}()
	before := addrs(sparse)
	if err := waitGC(finDone, "interior finalizer"); err != nil {
		return err
	}
	var clArg *uintptr
	deadline := time.After(30 * time.Second)
	for clArg == nil {
		select {
		case clArg = <-clGot:
		case <-deadline:
			return errors.New("cleanup with interior argument did not run")
		case <-time.After(2 * time.Millisecond):
			runtime.GC()
		}
	}
	if clArg != &sparse[iCl].pad[1] {
		return errors.New("cleanup interior argument is a stale copy")
	}
	if m[&sparse[iMap0].pad[3]] != iMap0 || m[&sparse[iMap1].pad[3]] != iMap1 {
		return errors.New("interior-pointer map lookup failed after collection")
	}
	if sparse[iMap0].pad[3] != mk0 || sparse[iMap1].pad[3] != mk1 {
		return errors.New("interior marker lost its value")
	}
	runtime.GC()
	churn()
	if err := requireSomeMoved(nodesMoved(before, sparse, 8)); err != nil {
		return err
	}
	return finish(before, sparse)
}

var gbox any

// boxesHeld keeps interface-held pointer-free boxes alive through ordinary
// map, channel, and global roots across a relocation cycle. Every root must
// still name the same box with the same contents: a stale box would hold
// churned garbage.
func boxesHeld() error {
	sparse := fragment(nil)
	boxes := fragmentBoxes()
	idx := [6]int{target(0), target(1), target(2), target(3), target(4), target(5)}
	gbox = boxes[idx[0]]
	m := map[int]any{1: boxes[idx[1]], 2: boxes[idx[2]]}
	ch := make(chan any, 2)
	ch <- boxes[idx[3]]
	ch <- boxes[idx[4]]
	var iface any = boxes[idx[5]]
	nodeAt, boxAt := addrs(sparse), boxWords(boxes)
	runtime.GC()
	churn()
	check := func(name string, v any, want int) error {
		b, ok := v.(blob)
		if !ok || b[0] != 1000000+uint64(want*keepEvery) {
			return fmt.Errorf("%s no longer names its box intact", name)
		}
		if boxWord(v) != boxWord(boxes[want]) {
			return fmt.Errorf("%s names a stale box", name)
		}
		return nil
	}
	c1, c2 := <-ch, <-ch
	got := [6]any{gbox, m[1], m[2], c1, c2, iface}
	names := [6]string{"global", "map 1", "map 2", "channel first", "channel second", "interface"}
	for k := range 6 {
		if err := check(names[k], got[k], idx[k]); err != nil {
			return err
		}
	}
	fmt.Printf("boxes moved=%d/%d ", boxesMoved(boxAt, boxes), len(boxes))
	if err := requireSomeMoved(boxesMovedAt(boxAt, boxes, 6)); err != nil {
		return err
	}
	return finish(nodeAt, sparse)
}

// bigNode is a large pointer-bearing object: a multi-kilobyte span the
// collector can evacuate, with a link that must follow its target.
type bigNode struct {
	link *node
	vals [511]uint64
}

// tinyVal is a tiny-allocator object: pointer-free and under maxTinySize, so
// several share one 16-byte tiny block.
type tinyVal struct{ v uint64 }

var (
	sinkBig *bigNode
	sinkNs  *[256]uint64
	sinkTn  *tinyVal
)

// fragmentBig mirrors fragmentBoxes for 4 KiB pointer-bearing objects: a
// group that dies, anchored by sinkBig so it reaches the heap, and a group
// of which 1 in keepEvery survives, so that the survivors' blocks are
// fragmented and the dead group leaves free slots to move them to.
func fragmentBig(linkTo []*node) []*bigNode {
	const n = 300
	dense := make([]*bigNode, n)
	for i := range dense {
		b := &bigNode{link: linkTo[target(i%8)]}
		b.vals[0] = uint64(2000000 + i)
		b.vals[256] = uint64(3000000 + i)
		b.vals[510] = uint64(4000000 + i)
		dense[i] = b
	}
	sinkBig = dense[0]
	keep := make([]*bigNode, 0, n/keepEvery)
	for i := range n {
		b := &bigNode{link: linkTo[target(i%8)]}
		b.vals[0] = uint64(2000000 + i)
		b.vals[256] = uint64(3000000 + i)
		b.vals[510] = uint64(4000000 + i)
		if i%keepEvery == 0 {
			keep = append(keep, b)
		}
	}
	return keep
}

// fragmentNoscan mirrors fragmentBoxes for 2 KiB pointer-free objects.
func fragmentNoscan() []*[256]uint64 {
	const n = 600
	dense := make([]*[256]uint64, n)
	for i := range dense {
		b := &[256]uint64{}
		b[0] = uint64(5000000 + i)
		b[128] = uint64(6000000 + i)
		b[255] = uint64(7000000 + i)
		dense[i] = b
	}
	sinkNs = dense[0]
	keep := make([]*[256]uint64, 0, n/keepEvery)
	for i := range n {
		b := &[256]uint64{}
		b[0] = uint64(5000000 + i)
		b[128] = uint64(6000000 + i)
		b[255] = uint64(7000000 + i)
		if i%keepEvery == 0 {
			keep = append(keep, b)
		}
	}
	return keep
}

// fragmentTiny mirrors fragmentBoxes for tiny-allocator objects.
func fragmentTiny() []*tinyVal {
	const n = 20000
	dense := make([]*tinyVal, n)
	for i := range dense {
		dense[i] = &tinyVal{v: uint64(8000000 + i)}
	}
	sinkTn = dense[0]
	keep := make([]*tinyVal, 0, n/keepEvery)
	for i := range n {
		v := &tinyVal{v: uint64(8000000 + i)}
		if i%keepEvery == 0 {
			keep = append(keep, v)
		}
	}
	return keep
}

// sizeMix puts large, noscan, and tiny-allocator objects through relocation
// cycles and requires their contents intact. Large spans stay classic and
// never move (the sizeclass-0 exclusion in logosSpanMovable), so the large
// object asserts stability with contents intact; the evacuable 4 KiB
// pointer-bearing and 2 KiB pointer-free spans must show movement under a
// moving collector, with links and payloads intact. Tiny blocks may move as
// whole 16-byte slots, so the tiny values assert contents intact and report
// movement without requiring it.
func sizeMix() error {
	sparse := fragment(nil)
	big := fragmentBig(sparse)
	ns := fragmentNoscan()
	tn := fragmentTiny()
	large := new([64 << 10]byte)
	for i := range large {
		large[i] = byte(i)
	}
	bigAt := make([]uintptr, len(big))
	for i, b := range big {
		bigAt[i] = uintptr(unsafe.Pointer(b))
	}
	nsAt := make([]uintptr, len(ns))
	for i, b := range ns {
		nsAt[i] = uintptr(unsafe.Pointer(b))
	}
	tnAt := make([]uintptr, len(tn))
	for i, v := range tn {
		tnAt[i] = uintptr(unsafe.Pointer(v))
	}
	largeAt := uintptr(unsafe.Pointer(large))
	nodeAt := addrs(sparse)
	runtime.GC()
	// Reuse the vacated slots with objects of the survivors' size classes,
	// so that a pointer left to a vacated slot reads another object's bytes.
	var junkBig []*bigNode
	var junkNs []*[256]uint64
	var junkTn []*tinyVal
	for range 300 {
		junkBig = append(junkBig, &bigNode{})
	}
	for range 600 {
		junkNs = append(junkNs, &[256]uint64{})
	}
	for range 2000 {
		junkTn = append(junkTn, &tinyVal{})
	}
	for j, b := range big {
		i := j * keepEvery
		if b.vals[0] != uint64(2000000+i) || b.vals[256] != uint64(3000000+i) || b.vals[510] != uint64(4000000+i) {
			return fmt.Errorf("large pointer-bearing survivor %d lost its contents", j)
		}
		if want := sparse[target(i%8)]; b.link != want || b.link.v != 1000000+target(i%8)*keepEvery {
			return fmt.Errorf("large pointer-bearing survivor %d lost its link", j)
		}
	}
	for j, b := range ns {
		i := j * keepEvery
		if b[0] != uint64(5000000+i) || b[128] != uint64(6000000+i) || b[255] != uint64(7000000+i) {
			return fmt.Errorf("noscan survivor %d lost its contents", j)
		}
	}
	movedTn := 0
	for j, v := range tn {
		if v.v != uint64(8000000+j*keepEvery) {
			return fmt.Errorf("tiny survivor %d lost its contents", j)
		}
		if uintptr(unsafe.Pointer(v)) != tnAt[j] {
			movedTn++
		}
	}
	if uintptr(unsafe.Pointer(large)) != largeAt {
		return errors.New("large object moved out of its classic span")
	}
	for i := range large {
		if large[i] != byte(i) {
			return fmt.Errorf("large object byte %d corrupt", i)
		}
	}
	movedBig := 0
	for i := range 8 {
		if uintptr(unsafe.Pointer(big[i])) != bigAt[i] {
			movedBig++
		}
	}
	if err := requireSomeMoved(movedBig); err != nil {
		return err
	}
	movedNs := 0
	for i := range 8 {
		if uintptr(unsafe.Pointer(ns[i])) != nsAt[i] {
			movedNs++
		}
	}
	if err := requireSomeMoved(movedNs); err != nil {
		return err
	}
	fmt.Printf("big moved=%d/%d noscan moved=%d/%d tiny moved=%d/%d large=%d ", movedBig, len(big), movedNs, len(ns), movedTn, len(tn), len(large))
	runtime.KeepAlive(junkBig)
	runtime.KeepAlive(junkNs)
	runtime.KeepAlive(junkTn)
	return finish(nodeAt, sparse)
}

// attachRace attaches finalizers and cleanups from many goroutines while
// collections relocate the heap around them. Each attachment must run
// exactly once with its arguments intact: a lost attachment never runs, a
// duplicated one runs twice, and a stale argument names a vacated slot. The
// attach protocol (copy-then-pin: zgcAttachEnsureCopy with the M-held
// addspecialStable commit) is what keeps each racing attachment on its
// object's home.
func attachRace() error {
	sparse := fragment(nil)
	const racers = 16
	finObjs := make([]*holder, racers)
	clObjs := make([]*holder, racers)
	var finRuns, clRuns [racers]atomic.Uint32
	for i := range racers {
		finObjs[i] = &holder{child: sparse[target(i)]}
		clObjs[i] = &holder{child: sparse[target(i+racers)]}
	}
	before := addrs(sparse)
	var stop atomic.Bool
	var gcDone sync.WaitGroup
	gcDone.Add(1)
	go func() {
		defer gcDone.Done()
		for !stop.Load() {
			runtime.GC()
		}
	}()
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range racers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			// The closures capture only the index and the background
			// chain, never the attached object: capturing the object
			// would keep it alive and its attachment would never run.
			runtime.SetFinalizer(finObjs[i], func(o *holder) {
				if o.child != sparse[target(i)] || o.child.v != 1000000+target(i)*keepEvery {
					panic("racing finalizer sees a stale object")
				}
				finRuns[i].Add(1)
			})
			runtime.AddCleanup(clObjs[i], func(arg *node) {
				if want := sparse[target(i+racers)]; arg != want || arg.v != 1000000+target(i+racers)*keepEvery {
					panic("racing cleanup sees a stale argument")
				}
				clRuns[i].Add(1)
			}, sparse[target(i+racers)])
		}(i)
	}
	close(start)
	wg.Wait()
	stop.Store(true)
	gcDone.Wait()
	clear(finObjs)
	clear(clObjs)
	deadline := time.After(60 * time.Second)
	for {
		runtime.GC()
		ok := true
		for i := range racers {
			if finRuns[i].Load() != 1 || clRuns[i].Load() != 1 {
				ok = false
				break
			}
		}
		if ok {
			break
		}
		select {
		case <-deadline:
			ran := 0
			for i := range racers {
				if finRuns[i].Load() == 1 {
					ran++
				}
				if clRuns[i].Load() == 1 {
					ran++
				}
			}
			return fmt.Errorf("only %d of %d racing attachments ran", ran, 2*racers)
		default:
		}
	}
	for range 3 {
		runtime.GC()
	}
	for i := range racers {
		if finRuns[i].Load() != 1 {
			return fmt.Errorf("racing finalizer %d ran %d times, want exactly once", i, finRuns[i].Load())
		}
		if clRuns[i].Load() != 1 {
			return fmt.Errorf("racing cleanup %d ran %d times, want exactly once", i, clRuns[i].Load())
		}
	}
	fmt.Printf("racing attachments ran=%d ", 2*racers)
	churn()
	if err := requireSomeMoved(nodesMoved(before, sparse, 8)); err != nil {
		return err
	}
	return finish(before, sparse)
}

// cbCOrigin holds a C-origin callback's argument and result words in a
// frame blocked in a system call for the whole C frame, and collects
// while relocation runs around it. Under the moving collector a frame
// blocked in C is scanned conservatively, so its referents stay pinned
// (the same seam the syscall scenario covers for one word): the
// argument, the result, and an interior pointer into the argument must
// keep their addresses with contents intact across repeated collections
// with churn, while unrelated fragmented survivors still move. After the
// frame returns, the goroutine rechecks its own copies for stale words
// and the chain must verify whole.
func cbCOrigin() error {
	sparse := fragment(nil)
	iArg, iRes := target(8), target(9)
	const (
		mkCbArg = uintptr(0xcb00c0de)
		mkCbRes = uintptr(0xcb00c1de)
	)
	sparse[iArg].pad[3] = mkCbArg
	sparse[iRes].pad[3] = mkCbRes
	var fds [2]int
	if err := syscall.Pipe(fds[:]); err != nil {
		return err
	}
	type frameAddrs struct{ arg, res, interior uintptr }
	held := make(chan frameAddrs, 1)
	resCh := make(chan error, 1)
	go func(a, r *node) {
		interior := &a.pad[3]
		held <- frameAddrs{uintptr(unsafe.Pointer(a)), uintptr(unsafe.Pointer(r)), uintptr(unsafe.Pointer(interior))}
		var b [1]byte
		syscall.Read(fds[0], b[:])
		switch {
		case a != sparse[iArg]:
			resCh <- errors.New("C-origin callback argument points to a stale copy")
		case r != sparse[iRes]:
			resCh <- errors.New("C-origin callback result points to a stale copy")
		case interior != &sparse[iArg].pad[3] || *interior != mkCbArg:
			resCh <- errors.New("C-origin callback lost its interior word")
		case a.v != 1000000+iArg*keepEvery || r.v != 1000000+iRes*keepEvery:
			resCh <- errors.New("C-origin callback word lost its contents")
		default:
			resCh <- nil
		}
		runtime.KeepAlive(a)
		runtime.KeepAlive(r)
	}(sparse[iArg], sparse[iRes])
	at := <-held
	time.Sleep(100 * time.Millisecond)
	before := addrs(sparse)
	for range 3 {
		runtime.GC()
	}
	churn()
	runtime.GC()
	if uintptr(unsafe.Pointer(sparse[iArg])) != at.arg {
		return errors.New("C-origin callback argument moved during its C frame")
	}
	if uintptr(unsafe.Pointer(sparse[iRes])) != at.res {
		return errors.New("C-origin callback result moved during its C frame")
	}
	if uintptr(unsafe.Pointer(&sparse[iArg].pad[3])) != at.interior {
		return errors.New("C-origin callback interior word moved during its C frame")
	}
	if sparse[iArg].pad[3] != mkCbArg || sparse[iRes].pad[3] != mkCbRes {
		return errors.New("C-origin callback word lost its marker")
	}
	if err := requireSomeMoved(nodesMoved(before, sparse, 8)); err != nil {
		return err
	}
	syscall.Write(fds[1], []byte{1})
	if err := <-resCh; err != nil {
		return err
	}
	return finish(before, sparse)
}

// cbPinnerSeparation keeps two objects alive with Pinners and one more
// with an empty Go-origin callback scope (a parked frame holding only
// ordinary roots) in the same function, then retires each lifetime
// independently in both directions. The scope object is referenced
// nowhere else. First one pin is released while the scope stays: the
// scope must keep its object alive and intact on its own, or the scope
// lifetime depends on the pin lifetime. Then the scope ends while the
// other pin, never released, stays: the scope object must become
// collectable while the kept pinned object stays address-stable. Either
// merged lifetime fails one of the two rows below: a pin that extended
// the scope would keep the scope object alive past its scope end, and a
// scope end that released pins would let the kept pinned object move.
// The kept pin is never released, so its object never leaves its
// fragmented span and the last row cannot pass vacuously.
func cbPinnerSeparation() error {
	sparse := fragment(nil)
	iPinKeep, iPinDrop, iLink := target(8), target(9), target(10)
	var pinKeep, pinDrop runtime.Pinner
	pinKeep.Pin(sparse[iPinKeep])
	defer pinKeep.Unpin()
	pinKeepAt := uintptr(unsafe.Pointer(sparse[iPinKeep]))
	pinDrop.Pin(sparse[iPinDrop])
	pinDropAt := uintptr(unsafe.Pointer(sparse[iPinDrop]))
	scope := &node{v: 424242, next: sparse[iLink]}
	weakScope := weak.Make(scope)
	parked := make(chan struct{})
	gate := make(chan struct{})
	scopeRes := make(chan error, 1)
	go func(s *node) {
		close(parked)
		<-gate
		if s.v != 424242 || s.next != sparse[iLink] {
			scopeRes <- errors.New("empty scope lost its retained word")
			return
		}
		scopeRes <- nil
	}(scope)
	// Drop the main reference: only the parked scope frame retains the
	// object now.
	scope = nil
	<-parked
	before := addrs(sparse)
	runtime.GC()
	if weakScope.Value() == nil {
		return errors.New("scope object died while its scope was active")
	}
	if uintptr(unsafe.Pointer(sparse[iPinKeep])) != pinKeepAt {
		return errors.New("kept pinned object moved while its pin was active")
	}
	if uintptr(unsafe.Pointer(sparse[iPinDrop])) != pinDropAt {
		return errors.New("dropped pinned object moved while its pin was active")
	}
	// Release one pin while the scope stays: the scope must keep its
	// object alive and intact on its own. A scope that depended on the
	// pin would lose its object here. The dropped object stays
	// reachable through sparse, so only its contents are checked.
	pinDrop.Unpin()
	runtime.GC()
	if s := weakScope.Value(); s == nil || s.v != 424242 || s.next != sparse[iLink] {
		return errors.New("scope object died with the pin: scope lifetime depends on pin lifetime")
	}
	if sparse[iPinDrop].v != 1000000+iPinDrop*keepEvery {
		return errors.New("unpinned object lost its contents")
	}
	// End the scope while the kept pin stays put. pinKeep was never
	// released, so its object is still in its fragmented span and a
	// scope end that released pins would let it move.
	close(gate)
	if err := <-scopeRes; err != nil {
		return err
	}
	for range 5 {
		runtime.GC()
	}
	if weakScope.Value() != nil {
		return errors.New("scope object survived its scope: pin lifetime extends scope lifetime")
	}
	if uintptr(unsafe.Pointer(sparse[iPinKeep])) != pinKeepAt {
		return errors.New("pinned object moved at scope end: scope lifetime owns pin lifetime")
	}
	if sparse[iPinKeep].v != 1000000+iPinKeep*keepEvery {
		return errors.New("pinned object lost its contents")
	}
	if err := requireSomeMoved(nodesMoved(before, sparse, 8)); err != nil {
		return err
	}
	return finish(before, sparse)
}

// cbRetire parks several callback-retained words (an argument, a result,
// and an interior pointer into the argument) in a C-origin frame for
// positive control, then returns from the callback and requires every
// retained word to become collectable. Nothing else references them, so
// a surviving weak pointer would mean the scope outlived its frame.
func cbRetire() error {
	sparse := fragment(nil)
	arg := &node{v: 111111, next: sparse[target(0)]}
	res := &node{v: 222222, next: sparse[target(1)]}
	const mkRetire = uintptr(0xcb01e7)
	arg.pad[3] = mkRetire
	wArg, wRes := weak.Make(arg), weak.Make(res)
	var fds [2]int
	if err := syscall.Pipe(fds[:]); err != nil {
		return err
	}
	held := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func(a, r *node) {
		interior := &a.pad[3]
		held <- struct{}{}
		var b [1]byte
		syscall.Read(fds[0], b[:])
		switch {
		case a.v != 111111 || a.next != sparse[target(0)]:
			done <- errors.New("callback frame lost its argument")
		case r.v != 222222 || r.next != sparse[target(1)]:
			done <- errors.New("callback frame lost its result")
		case interior != &a.pad[3] || *interior != mkRetire:
			done <- errors.New("callback frame lost its interior word")
		default:
			done <- nil
		}
		runtime.KeepAlive(a)
		runtime.KeepAlive(r)
	}(arg, res)
	// Drop the main references: only the callback frame retains the
	// words now.
	arg, res = nil, nil
	<-held
	time.Sleep(100 * time.Millisecond)
	before := addrs(sparse)
	runtime.GC()
	churn()
	if wArg.Value() == nil || wRes.Value() == nil {
		return errors.New("retained word died inside its callback frame")
	}
	if err := requireSomeMoved(nodesMoved(before, sparse, 8)); err != nil {
		return err
	}
	syscall.Write(fds[1], []byte{1})
	if err := <-done; err != nil {
		return err
	}
	for range 5 {
		runtime.GC()
	}
	if wArg.Value() != nil || wRes.Value() != nil {
		return errors.New("retained word survived its callback frame")
	}
	return finish(before, sparse)
}

var scenarios = map[string]func() error{
	"canmove":        canMove,
	"evacuate":       evacuate,
	"pinner":         pinner,
	"mapkey":         mapKey,
	"syscall":        conservativeFrame,
	"weakunique":     weakUnique,
	"finreg":         finalizerRegistration,
	"finqueued":      finalizerQueued,
	"finresurrect":   finalizerResurrection,
	"finreclaim":     finalizerReclaims,
	"cleanupmoved":   cleanupMoved,
	"cleanupstop":    cleanupStop,
	"deferheld":      deferHeld,
	"panicheld":      panicHeld,
	"ctxheld":        contextHeld,
	"rootspreserved": rootsPreserved,
	"interiorheld":   interiorHeld,
	"boxroots":       boxesHeld,
	"sizemix":        sizeMix,
	"attachrace":     attachRace,
	"cbcorigin":      cbCOrigin,
	"cbpinnersep":    cbPinnerSeparation,
	"cbretire":       cbRetire,
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
