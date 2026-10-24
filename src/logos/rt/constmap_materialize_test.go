// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package rt_test

import (
	"logos/rt"
	"sync"
	"sync/atomic"
	"testing"
)

func TestConstMapMaterializationReusesMap(t *testing.T) {
	var calls atomic.Int32
	var storage rt.ConstMapMaterialization[map[string]int, string, int]
	build := func() map[string]int {
		calls.Add(1)
		return map[string]int{"a": 1}
	}
	first := storage.Get(build)
	second := storage.Get(build)
	if calls.Load() != 1 {
		t.Fatalf("build ran %d times, want 1", calls.Load())
	}
	first["b"] = 2
	if second["b"] != 2 {
		t.Fatalf("second Get missed mutation, want shared map")
	}
}

func TestConstMapMaterializationConcurrent(t *testing.T) {
	var storage rt.ConstMapMaterialization[map[int]int, int, int]
	var calls atomic.Int32
	build := func() map[int]int {
		calls.Add(1)
		return map[int]int{1: 10}
	}
	const workers = 32
	var wg sync.WaitGroup
	results := make([]map[int]int, workers)
	for i := range workers {
		wg.Go(func() {
			results[i] = storage.Get(build)
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("concurrent build ran %d times, want 1", calls.Load())
	}
	for i := 1; i < workers; i++ {
		results[0][i] = i
	}
	for i := 1; i < workers; i++ {
		if results[i][i] != i {
			t.Fatalf("worker %d missed shared mutation", i)
		}
	}
}

func TestConstMapMaterializationPanicRepeats(t *testing.T) {
	var storage rt.ConstMapMaterialization[map[string]int, string, int]
	sentinel := &struct{ msg string }{msg: "boom"}
	var calls atomic.Int32
	build := func() map[string]int {
		calls.Add(1)
		panic(sentinel)
	}
	for range 2 {
		func() {
			defer func() {
				r := recover()
				if r != sentinel {
					t.Fatalf("panic = %v, want sentinel", r)
				}
			}()
			storage.Get(build)
			t.Fatal("Get did not panic")
		}()
	}
	if calls.Load() != 1 {
		t.Fatalf("panicking build ran %d times, want 1", calls.Load())
	}
}

func TestConstMapMaterializationPanicConcurrent(t *testing.T) {
	var storage rt.ConstMapMaterialization[map[int]int, int, int]
	sentinel := &struct{ msg string }{msg: "bang"}
	build := func() map[int]int {
		panic(sentinel)
	}
	const workers = 16
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			defer func() {
				if recover() != any(sentinel) {
					t.Error("concurrent panic mismatch")
				}
			}()
			storage.Get(build)
		})
	}
	wg.Wait()
}

type testNamedMap map[string]int

func TestConstMapMaterializationNamedMap(t *testing.T) {
	var storage rt.ConstMapMaterialization[testNamedMap, string, int]
	build := func() testNamedMap {
		return testNamedMap{"k": 7}
	}
	got := storage.Get(build)
	if got["k"] != 7 {
		t.Fatalf("named map value = %d, want 7", got["k"])
	}
	var _ testNamedMap = got
}
