// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package json

import (
	"math"
	"math/rand/v2"
	"strconv"
	"testing"
)

func intBoundaries() []int64 {
	out := []int64{0, 1, -1, math.MaxInt64, math.MinInt64, math.MaxInt64 - 1, math.MinInt64 + 1}
	p := int64(1)
	for range 19 {
		out = append(out, p-1, p, p+1, -(p - 1), -p, -(p + 1))
		if p > math.MaxInt64/10 {
			break
		}
		p *= 10
	}
	for _, v := range []int64{math.MaxInt8, math.MinInt8, math.MaxInt16, math.MinInt16, math.MaxInt32, math.MinInt32} {
		out = append(out, v, v+1, v-1)
	}
	return out
}

func uintBoundaries() []uint64 {
	out := []uint64{0, 1, 9, 10, math.MaxUint64, math.MaxUint64 - 1, math.MaxInt64, math.MaxInt64 + 1}
	p := uint64(1)
	for range 20 {
		out = append(out, p-1, p, p+1)
		if p > math.MaxUint64/10 {
			break
		}
		p *= 10
	}
	return out
}

func TestIntLenBoundaries(t *testing.T) {
	for _, v := range intBoundaries() {
		want := len(strconv.AppendInt(nil, v, 10))
		if got := IntLen(v); got != want {
			t.Errorf("IntLen(%d) = %d, want %d", v, got, want)
		}
		if got := string(AppendInt(nil, v)); got != strconv.FormatInt(v, 10) {
			t.Errorf("AppendInt(%d) = %q", v, got)
		}
	}
}

func TestUintLenBoundaries(t *testing.T) {
	for _, v := range uintBoundaries() {
		want := len(strconv.AppendUint(nil, v, 10))
		if got := UintLen(v); got != want {
			t.Errorf("UintLen(%d) = %d, want %d", v, got, want)
		}
		if got := string(AppendUint(nil, v)); got != strconv.FormatUint(v, 10) {
			t.Errorf("AppendUint(%d) = %q", v, got)
		}
	}
}

func TestIntLenSweep(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for range 200000 {
		u := r.Uint64() >> r.UintN(64)
		if got, want := UintLen(u), len(strconv.AppendUint(nil, u, 10)); got != want {
			t.Fatalf("UintLen(%d) = %d, want %d", u, got, want)
		}
		v := int64(u)
		if r.UintN(2) == 0 {
			v = -v
		}
		if got, want := IntLen(v), len(strconv.AppendInt(nil, v, 10)); got != want {
			t.Fatalf("IntLen(%d) = %d, want %d", v, got, want)
		}
	}
}

func TestAppendIntKeepsPrefix(t *testing.T) {
	if got := string(AppendInt([]byte("n="), -42)); got != "n=-42" {
		t.Errorf("AppendInt = %q", got)
	}
	if got := string(AppendUint([]byte("n="), 42)); got != "n=42" {
		t.Errorf("AppendUint = %q", got)
	}
}
