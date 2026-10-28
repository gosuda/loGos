// run -goexperiment logosgcmoving -gcflags=-lb

//go:build amd64 || arm64

// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// M19-S7 compiler attack (SPEC Sec 8.4, T-301.2): a program compiled with
// the zgc load barrier (-lb) runs unchanged on the stock collector while
// zgc is not configured. Every pointer load of this package passes the
// barrier: list and tree links, map values, interface data words, closure
// captures, string and slice data, atomic pointers, and channel payloads,
// while collections run concurrently.

package main

import (
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
)

type node struct {
	next *node
	val  int
}

type tree struct {
	left, right *tree
	key         string
}

func (t *tree) insert(k string) *tree {
	if t == nil {
		return &tree{key: k}
	}
	if k < t.key {
		t.left = t.left.insert(k)
	} else {
		t.right = t.right.insert(k)
	}
	return t
}

func (t *tree) walk(out *[]string) {
	if t == nil {
		return
	}
	t.left.walk(out)
	*out = append(*out, t.key)
	t.right.walk(out)
}

type shape interface{ area() int }

type rect struct{ w, h *int }

func (r rect) area() int { return *r.w * *r.h }

func check(cond bool, msg string, args ...any) {
	if !cond {
		panic(fmt.Sprintf(msg, args...))
	}
}

//go:noinline
func secondVal(pp **node) int { return (*pp).next.val }

func nested() {
	n := &node{next: &node{val: 42}}
	check(secondVal(&n) == 42, "nested load")
}

// joinLoop loads p.next after a join of two paths, one of which stores: the
// load takes its barrier behind the memory phi of the join and the loop head.
//
//go:noinline
func joinLoop(head *node) (evens, odds int) {
	for p := head; p != nil; p = p.next {
		if p.val%2 == 0 {
			p.val += 100
			evens++
		} else {
			odds++
		}
	}
	return
}

func joins() {
	var head *node
	for i := range 100 {
		head = &node{next: head, val: i}
	}
	evens, odds := joinLoop(head)
	check(evens == 50 && odds == 50, "join counts %d %d", evens, odds)
	sum := 0
	for n := head; n != nil; n = n.next {
		sum += n.val
	}
	check(sum == 100*99/2+50*100, "join stores %d", sum)
}

// reverse stores the pointer it has just loaded: the store must follow the
// barrier of the load, and the load must still read the old link.
//
//go:noinline
func reverse(head *node) *node {
	var prev *node
	for head != nil {
		next := head.next
		head.next = prev
		prev, head = head, next
	}
	return prev
}

func reversal() {
	var head *node
	for i := range 1000 {
		head = &node{next: head, val: i}
	}
	runtime.GC()
	head = reverse(head)
	runtime.GC()
	i := 0
	for n := head; n != nil; n = n.next {
		check(n.val == i, "reversed value %d at %d", n.val, i)
		i++
	}
	check(i == 1000, "reversed length %d", i)
}

func lists() {
	var head *node
	for i := range 10000 {
		head = &node{next: head, val: i}
		if i%1000 == 0 {
			runtime.GC()
		}
	}
	sum := 0
	for n := head; n != nil; n = n.next {
		sum += n.val
	}
	check(sum == 10000*9999/2, "list sum %d", sum)
}

func trees() {
	var root *tree
	var keys []string
	for i := range 2000 {
		k := fmt.Sprintf("k%05d", (i*7919)%2000)
		keys = append(keys, k)
		root = root.insert(k)
	}
	runtime.GC()
	var got []string
	root.walk(&got)
	slices.Sort(keys)
	check(strings.Join(got, ",") == strings.Join(keys, ","), "tree order")
}

func maps() {
	m := make(map[string]*node)
	for i := range 5000 {
		m[fmt.Sprint(i)] = &node{val: i}
	}
	runtime.GC()
	for i := range 5000 {
		check(m[fmt.Sprint(i)].val == i, "map value %d", i)
	}
	byPtr := make(map[*node]int)
	for _, v := range m {
		byPtr[v] = v.val
	}
	for k, v := range byPtr {
		check(k.val == v, "pointer key %d", v)
	}
}

func interfaces() {
	var shapes []shape
	for i := range 1000 {
		w, h := i, 2
		shapes = append(shapes, rect{&w, &h})
	}
	runtime.GC()
	for i, s := range shapes {
		check(s.area() == 2*i, "area %d", i)
	}
}

func closures() {
	var fns []func() int
	for i := range 1000 {
		p := &node{val: i}
		fns = append(fns, func() int { return p.val })
	}
	runtime.GC()
	for i, f := range fns {
		check(f() == i, "closure %d", i)
	}
}

func atomics() {
	var p atomic.Pointer[node]
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for j := range 1000 {
				p.Store(&node{val: i*1000 + j})
				if n := p.Load(); n == nil || n.val < 0 {
					panic("atomic load")
				}
			}
		})
	}
	wg.Wait()
}

func channels() {
	ch := make(chan *node, 16)
	go func() {
		for i := range 5000 {
			ch <- &node{val: i}
		}
		close(ch)
	}()
	sum := 0
	for n := range ch {
		sum += n.val
	}
	check(sum == 5000*4999/2, "channel sum %d", sum)
}

func main() {
	nested()
	joins()
	reversal()
	lists()
	trees()
	maps()
	interfaces()
	closures()
	atomics()
	channels()
}
