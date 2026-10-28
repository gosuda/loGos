// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"os"
	"runtime"
	"runtime/metrics"
	"sync"
	"sync/atomic"
	"time"
)

// Helper programs of the M19-S2 pacer attacks (src/runtime/logos_pacer_test.go).
// The attacks run them with GODEBUG=gcpacertrace=1 and read the pacer lines
// from the output.

func init() {
	register("LogosPacerElastic", LogosPacerElastic)
	register("LogosPacerAssist", LogosPacerAssist)
	register("LogosPacerForced", LogosPacerForced)
}

type logosPacerNode struct {
	next *logosPacerNode
	pad  [5]*logosPacerNode
}

var (
	logosPacerLive *logosPacerNode
	logosPacerSink *logosPacerNode
)

// LogosPacerElastic keeps a large live list and allocates garbage until the
// collector has started several cycles, so every cycle start prints its
// worker split.
func LogosPacerElastic() {
	runtime.GOMAXPROCS(4)
	nodes, cycles := 1<<20, uint64(6)
	if os.Getenv("RUNTIME_TEST_SHORT") != "" {
		nodes, cycles = 1<<19, 3
	}
	for range nodes {
		logosPacerLive = &logosPacerNode{next: logosPacerLive}
	}
	sample := []metrics.Sample{{Name: "/gc/cycles/total:gc-cycles"}}
	collections := func() uint64 {
		metrics.Read(sample)
		return sample[0].Value.Uint64()
	}
	want := collections() + cycles
	deadline := time.Now().Add(60 * time.Second)
	for collections() < want && time.Now().Before(deadline) {
		for range 1000 {
			logosPacerSink = &logosPacerNode{}
		}
	}
	if collections() < want {
		fmt.Println("FAIL: too few collections")
		return
	}
	fmt.Println("OK")
}

// LogosPacerAssist allocates from more goroutines than Ps against a large
// live list and waits until mark assist has run, read from
// /cpu/classes/gc/mark/assist:cpu-seconds: assist is the backstop of the
// elastic background workers.
func LogosPacerAssist() {
	runtime.GOMAXPROCS(2)
	nodes := 1 << 19
	if os.Getenv("RUNTIME_TEST_SHORT") != "" {
		nodes = 1 << 18
	}
	for range nodes {
		logosPacerLive = &logosPacerNode{next: logosPacerLive}
	}
	var stop atomic.Bool
	var wg sync.WaitGroup
	sinks := make([]*logosPacerNode, 8)
	for i := range sinks {
		wg.Go(func() {
			for !stop.Load() {
				sinks[i] = &logosPacerNode{}
			}
		})
	}
	sample := []metrics.Sample{{Name: "/cpu/classes/gc/mark/assist:cpu-seconds"}}
	deadline := time.Now().Add(30 * time.Second)
	assisted := false
	for !assisted && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		metrics.Read(sample)
		assisted = sample[0].Value.Float64() > 0
	}
	stop.Store(true)
	wg.Wait()
	if !assisted {
		fmt.Println("FAIL: no mark assist ran")
		return
	}
	fmt.Println("OK")
}

// LogosPacerForced starts two collections by hand. It is the helper for
// collector modes that start no cycle of their own, where the attack needs
// only the pacer line of a cycle start.
func LogosPacerForced() {
	runtime.GC()
	runtime.GC()
	fmt.Println("OK")
}
