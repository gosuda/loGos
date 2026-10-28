// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package rt_test

import (
	"reflect"
	"testing"
	"unsafe"

	"logos/rt"
)

func assertFreezePanics[T any](t *testing.T, name string, src *T) {
	t.Helper()
	var (
		panicked  bool
		completed bool
		returned  *T
	)
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
			}
		}()
		returned = rt.Freeze(src)
		completed = true
	}()
	if !panicked {
		t.Fatalf("%s: expected Freeze to panic/reject unsupported leaf, but it returned %v", name, returned)
	}
	if completed || returned != nil {
		t.Fatalf("%s: rejection did not abort snapshot construction: completed=%v, returned=%v", name, completed, returned)
	}
}

func TestFreezeRejectsReachableFunc(t *testing.T) {
	t.Run("Direct", func(t *testing.T) {
		fn := func() {}
		assertFreezePanics(t, "direct non-nil func", &fn)
	})

	t.Run("ClosureOverMutableState", func(t *testing.T) {
		captured := false
		closure := func() {
			captured = !captured
		}
		assertFreezePanics(t, "closure over mutable state", &closure)
		if captured {
			t.Fatal("closure was invoked during test setup")
		}
	})

	t.Run("StructFieldExported", func(t *testing.T) {
		type funcStruct struct {
			Fn func()
		}
		s := funcStruct{Fn: func() {}}
		assertFreezePanics(t, "exported struct field func", &s)
	})

	t.Run("StructFieldUnexported", func(t *testing.T) {
		type unexpFuncStruct struct {
			fn func(int) int
		}
		s := unexpFuncStruct{fn: func(x int) int { return x + 1 }}
		assertFreezePanics(t, "unexported struct field func", &s)
	})

	t.Run("SliceElement", func(t *testing.T) {
		sl := []func(){func() {}}
		assertFreezePanics(t, "slice of func", &sl)
	})

	t.Run("ArrayElement", func(t *testing.T) {
		arr := [2]func(){func() {}, nil}
		assertFreezePanics(t, "array with non-nil func", &arr)
	})

	t.Run("MapValue", func(t *testing.T) {
		m := map[string]func(){"key": func() {}}
		assertFreezePanics(t, "map with func value", &m)
	})

	t.Run("InterfacePayload", func(t *testing.T) {
		var iface any = func() {}
		assertFreezePanics(t, "interface holding non-nil func", &iface)
	})

	t.Run("SliceCapTail", func(t *testing.T) {
		raw := make([]func(), 1, 3)
		raw = raw[:3]
		raw[2] = func() {}
		view := raw[:1]
		if len(view) != 1 || cap(view) != 3 {
			t.Fatalf("setup view len=%d cap=%d, want 1, 3", len(view), cap(view))
		}
		assertFreezePanics(t, "slice capacity tail non-nil func", &view)
	})

	t.Run("SliceCapTailAny", func(t *testing.T) {
		mut := false
		rawAny := make([]any, 1, 2)
		rawAny = rawAny[:2]
		rawAny[1] = func() { mut = !mut }
		viewAny := rawAny[:1]
		assertFreezePanics(t, "slice capacity tail closure in any", &viewAny)
		if mut {
			t.Fatal("closure was invoked during test setup")
		}
	})
}

func TestFreezeRejectsReachableChan(t *testing.T) {
	t.Run("Direct", func(t *testing.T) {
		ch := make(chan int, 1)
		assertFreezePanics(t, "direct non-nil chan", &ch)
	})

	t.Run("StructFieldExported", func(t *testing.T) {
		type chanStruct struct {
			Ch chan string
		}
		s := chanStruct{Ch: make(chan string)}
		assertFreezePanics(t, "exported struct field chan", &s)
	})

	t.Run("StructFieldUnexported", func(t *testing.T) {
		type unexpChanStruct struct {
			ch chan struct{}
		}
		s := unexpChanStruct{ch: make(chan struct{})}
		assertFreezePanics(t, "unexported struct field chan", &s)
	})

	t.Run("SliceElement", func(t *testing.T) {
		sl := []chan int{make(chan int)}
		assertFreezePanics(t, "slice with non-nil chan", &sl)
	})

	t.Run("ArrayElement", func(t *testing.T) {
		arr := [1]chan int{make(chan int)}
		assertFreezePanics(t, "array with non-nil chan", &arr)
	})

	t.Run("MapKey", func(t *testing.T) {
		chKey := make(chan int)
		mKey := map[chan int]int{chKey: 1}
		assertFreezePanics(t, "map with chan key", &mKey)
	})

	t.Run("MapValue", func(t *testing.T) {
		chVal := make(chan int)
		mVal := map[string]chan int{"c": chVal}
		assertFreezePanics(t, "map with chan value", &mVal)
	})

	t.Run("InterfacePayload", func(t *testing.T) {
		var iface any = make(chan int, 2)
		assertFreezePanics(t, "interface holding chan", &iface)
	})

	t.Run("SliceCapTail", func(t *testing.T) {
		raw := make([]chan int, 1, 2)
		raw = raw[:2]
		raw[1] = make(chan int)
		view := raw[:1]
		assertFreezePanics(t, "slice capacity tail non-nil chan", &view)
	})

	t.Run("SliceCapTailAny", func(t *testing.T) {
		rawAny := make([]any, 1, 2)
		rawAny = rawAny[:2]
		rawAny[1] = make(chan struct{})
		viewAny := rawAny[:1]
		assertFreezePanics(t, "slice capacity tail chan in any", &viewAny)
	})
}

func TestFreezeRejectsReachableUnsafePointer(t *testing.T) {
	t.Run("Direct", func(t *testing.T) {
		target := 42
		uptr := unsafe.Pointer(&target)
		assertFreezePanics(t, "direct unsafe.Pointer", &uptr)
	})

	t.Run("StructFieldExported", func(t *testing.T) {
		target := "real allocated target"
		type ptrStruct struct {
			P unsafe.Pointer
		}
		s := ptrStruct{P: unsafe.Pointer(&target)}
		assertFreezePanics(t, "exported struct field unsafe.Pointer", &s)
	})

	t.Run("StructFieldUnexported", func(t *testing.T) {
		target := [4]byte{1, 2, 3, 4}
		type unexpPtrStruct struct {
			p unsafe.Pointer
		}
		s := unexpPtrStruct{p: unsafe.Pointer(&target)}
		assertFreezePanics(t, "unexported struct field unsafe.Pointer", &s)
	})

	t.Run("SliceElement", func(t *testing.T) {
		target := 3.14159
		sl := []unsafe.Pointer{unsafe.Pointer(&target)}
		assertFreezePanics(t, "slice with unsafe.Pointer", &sl)
	})

	t.Run("ArrayElement", func(t *testing.T) {
		target := int64(987654321)
		arr := [1]unsafe.Pointer{unsafe.Pointer(&target)}
		assertFreezePanics(t, "array with unsafe.Pointer", &arr)
	})

	t.Run("MapKey", func(t *testing.T) {
		targetK := 100
		mKey := map[unsafe.Pointer]int{unsafe.Pointer(&targetK): 1}
		assertFreezePanics(t, "map with unsafe.Pointer key", &mKey)
	})

	t.Run("MapValue", func(t *testing.T) {
		targetV := 200
		mVal := map[string]unsafe.Pointer{"ptr": unsafe.Pointer(&targetV)}
		assertFreezePanics(t, "map with unsafe.Pointer value", &mVal)
	})

	t.Run("InterfacePayload", func(t *testing.T) {
		target := "interface target"
		var iface any = unsafe.Pointer(&target)
		assertFreezePanics(t, "interface holding unsafe.Pointer", &iface)
	})

	t.Run("SliceCapTail", func(t *testing.T) {
		target := 300
		raw := make([]unsafe.Pointer, 1, 2)
		raw = raw[:2]
		raw[1] = unsafe.Pointer(&target)
		view := raw[:1]
		assertFreezePanics(t, "slice capacity tail unsafe.Pointer", &view)
	})

	t.Run("SliceCapTailAny", func(t *testing.T) {
		target := 400
		rawAny := make([]any, 1, 2)
		rawAny = rawAny[:2]
		rawAny[1] = unsafe.Pointer(&target)
		viewAny := rawAny[:1]
		assertFreezePanics(t, "slice capacity tail unsafe.Pointer in any", &viewAny)
	})
}

func TestFreezeRejectsNestedUnsupportedLeaves(t *testing.T) {
	t.Run("PointerToStructUnexportedChan", func(t *testing.T) {
		type inner struct {
			c chan int
		}
		type outer struct {
			next *inner
		}
		val := &outer{next: &inner{c: make(chan int)}}
		assertFreezePanics(t, "pointer to struct with unexported chan", &val)
	})

	t.Run("MapHoldingSliceOfClosures", func(t *testing.T) {
		state := false
		m := map[string][]func(){
			"fns": {func() { state = !state }},
		}
		assertFreezePanics(t, "map holding slice of closures", &m)
		if state {
			t.Fatal("closure was invoked during test setup")
		}
	})

	t.Run("InterfaceHoldingCapTailUnsafePointer", func(t *testing.T) {
		target := "leaf"
		raw := make([]unsafe.Pointer, 1, 2)
		raw = raw[:2]
		raw[1] = unsafe.Pointer(&target)
		view := raw[:1]
		type container struct {
			Payload any
		}
		c := container{Payload: view}
		assertFreezePanics(t, "interface holding slice with cap tail unsafe.Pointer", &c)
	})
}

func TestFreezePreservesNilLeaves(t *testing.T) {
	t.Run("DirectNils", func(t *testing.T) {
		var fn func()
		gotFn := rt.Freeze(&fn)
		if gotFn == nil {
			t.Fatal("Freeze(&nilFn) returned nil pointer; want non-nil pointer to nil func")
		}
		if *gotFn != nil {
			t.Fatal("Freeze changed a nil function into a non-nil function")
		}

		var ch chan int
		gotCh := rt.Freeze(&ch)
		if gotCh == nil {
			t.Fatal("Freeze(&nilCh) returned nil pointer; want non-nil pointer to nil chan")
		}
		if *gotCh != nil {
			t.Fatalf("*Freeze(&nilCh) = %v, want nil", *gotCh)
		}

		var up unsafe.Pointer
		gotUp := rt.Freeze(&up)
		if gotUp == nil {
			t.Fatal("Freeze(&nilUp) returned nil pointer; want non-nil pointer to nil unsafe.Pointer")
		}
		if *gotUp != nil {
			t.Fatalf("*Freeze(&nilUp) = %v, want nil", *gotUp)
		}
	})

	t.Run("StructFieldsPreserveNil", func(t *testing.T) {
		type nilHolder struct {
			Name   string
			Fn     func()
			Ch     chan int
			Ptr    unsafe.Pointer
			fn     func(int) bool
			ch     chan string
			ptr    unsafe.Pointer
			AnyNil any
		}
		src := &nilHolder{
			Name:   "sound",
			Fn:     nil,
			Ch:     nil,
			Ptr:    nil,
			fn:     nil,
			ch:     nil,
			ptr:    nil,
			AnyNil: nil,
		}
		got := rt.Freeze(src)
		if got == src {
			t.Fatal("Freeze returned identical input pointer; want deep copy")
		}
		if got.Name != "sound" {
			t.Fatalf("got.Name = %q, want sound", got.Name)
		}
		if got.Fn != nil || got.Ch != nil || got.Ptr != nil || got.fn != nil || got.ch != nil || got.ptr != nil {
			t.Fatalf("frozen struct grew non-nil leaves: %+v", got)
		}
		if got.AnyNil != nil {
			t.Fatalf("frozen struct AnyNil grew non-nil: %v", got.AnyNil)
		}

		src.Name = "mutated"
		if got.Name != "sound" {
			t.Fatalf("frozen struct mutated with source: %q", got.Name)
		}
	})

	t.Run("SlicesAndCapTailsPreserveNil", func(t *testing.T) {
		sFn := make([]func(), 1, 3)
		gotSFn := *rt.Freeze(&sFn)
		if len(gotSFn) != 1 || cap(gotSFn) != 3 {
			t.Fatalf("nil func slice len=%d cap=%d, want 1, 3", len(gotSFn), cap(gotSFn))
		}
		for i := range cap(gotSFn) {
			if gotSFn[:cap(gotSFn)][i] != nil {
				t.Fatalf("nil func slice tail element %d became non-nil", i)
			}
		}

		sCh := make([]chan int, 1, 3)
		gotSCh := *rt.Freeze(&sCh)
		if len(gotSCh) != 1 || cap(gotSCh) != 3 {
			t.Fatalf("nil chan slice len=%d cap=%d, want 1, 3", len(gotSCh), cap(gotSCh))
		}
		for i := range cap(gotSCh) {
			if gotSCh[:cap(gotSCh)][i] != nil {
				t.Fatalf("nil chan slice tail element %d became non-nil", i)
			}
		}

		sPtr := make([]unsafe.Pointer, 1, 3)
		gotSPtr := *rt.Freeze(&sPtr)
		if len(gotSPtr) != 1 || cap(gotSPtr) != 3 {
			t.Fatalf("nil ptr slice len=%d cap=%d, want 1, 3", len(gotSPtr), cap(gotSPtr))
		}
		for i := range cap(gotSPtr) {
			if gotSPtr[:cap(gotSPtr)][i] != nil {
				t.Fatalf("nil ptr slice tail element %d became non-nil", i)
			}
		}
	})

	t.Run("MapsPreserveNilValues", func(t *testing.T) {
		mFn := map[string]func(){"f": nil}
		gotMFn := *rt.Freeze(&mFn)
		if gotMFn["f"] != nil {
			t.Fatal("map nil func value became non-nil")
		}

		mCh := map[string]chan int{"c": nil}
		gotMCh := *rt.Freeze(&mCh)
		if gotMCh["c"] != nil {
			t.Fatal("map nil chan value became non-nil")
		}

		mPtr := map[string]unsafe.Pointer{"p": nil}
		gotMPtr := *rt.Freeze(&mPtr)
		if gotMPtr["p"] != nil {
			t.Fatal("map nil unsafe.Pointer value became non-nil")
		}
	})
}

func TestFreezeIndependenceAfterRejection(t *testing.T) {
	captured := false
	badClosure := func() { captured = !captured }
	assertFreezePanics(t, "closure before recovery", &badClosure)
	if captured {
		t.Fatal("badClosure ran unexpectedly")
	}

	badSlice := make([]chan int, 1, 2)
	badSlice = badSlice[:2]
	badSlice[1] = make(chan int)
	badTail := badSlice[:1]
	assertFreezePanics(t, "chan in slice cap tail before recovery", &badTail)

	target := 555
	badUptr := unsafe.Pointer(&target)
	assertFreezePanics(t, "unsafe.Pointer before recovery", &badUptr)

	cfg := freezeSample()
	frozenCfg := rt.Freeze(cfg)
	if frozenCfg == cfg {
		t.Fatal("Freeze returned identical input pointer after recovery; want fresh copy")
	}
	if !reflect.DeepEqual(frozenCfg, cfg) {
		t.Fatalf("recovered Freeze = %+v, want deep-equal %+v", frozenCfg, cfg)
	}

	cfg.name = "mutated_after_panic"
	cfg.n = 999
	cfg.tags[0] = "MUTATED"
	cfg.labels["k"] = "MUTATED"
	cfg.child.n = 888

	if frozenCfg.name != "orig" || frozenCfg.n != 7 {
		t.Fatalf("frozen scalars changed after source mutation: %+v", frozenCfg)
	}
	if !reflect.DeepEqual(frozenCfg.tags, []string{"a", "b"}) {
		t.Fatalf("frozen tags changed after source mutation: %q", frozenCfg.tags)
	}
	if !reflect.DeepEqual(frozenCfg.labels, map[string]string{"k": "v"}) {
		t.Fatalf("frozen labels changed after source mutation: %v", frozenCfg.labels)
	}
	if frozenCfg.child.n != 1 {
		t.Fatalf("frozen child changed after source mutation: %d", frozenCfg.child.n)
	}
}
