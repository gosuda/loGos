// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && linux && (amd64 || arm64)

package runtime_test

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"

	. "runtime"
)

const logosZgcEFAULT = -14

func TestLogosZgcSyscallUntag(t *testing.T) {
	LogosZgcSelectSoftware(LogosZgcRemapped)
	base, free := logosZgcMapPages(1)
	defer free()
	for _, c := range logosZgcColors {
		LogosZgcFill(base, 64, 0xFF)
		colored := LogosZgcColored(base, c)
		if n := LogosZgcReadZero(colored, 64); n != logosZgcEFAULT {
			t.Fatalf("color %#x: kernel read through %#x returned %d, want EFAULT", c, colored, n)
		}
		if LogosZgcAllZero(base, 64) {
			t.Fatalf("color %#x: kernel wrote through a colored pointer", c)
		}
		clean := LogosZgcUntag(colored)
		if clean != base {
			t.Fatalf("color %#x: untag(%#x) = %#x, want %#x", c, colored, clean, base)
		}
		if n := LogosZgcReadZero(clean, 64); n != 64 || !LogosZgcAllZero(base, 64) {
			t.Fatalf("color %#x: kernel read through the untagged pointer returned %d", c, n)
		}
	}
}

func logosZgcRSSKB() int {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return -1
	}
	f := strings.Fields(string(data))
	if len(f) < 2 {
		return -1
	}
	pages, _ := strconv.Atoi(f[1])
	return pages * os.Getpagesize() / 1024
}

func logosZgcReportRSS(b *testing.B) {
	const size = 32 << 20
	before := logosZgcRSSKB()
	base, free := logosZgcMapPages(size / LogosZgcPageSize)
	LogosZgcFill(base, size, 1)
	b.ReportMetric(float64(logosZgcRSSKB()-before), "rss-single-KB")
	free()
	if multi, ok := logosZgcMultiMapRSS(size); ok {
		b.ReportMetric(float64(multi), "rss-multimap-KB")
	}
}

func logosZgcMultiMapRSS(size int) (kb int, ok bool) {
	f, err := os.CreateTemp("/dev/shm", "logos-zgc-*")
	if err != nil {
		return 0, false
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if f.Truncate(int64(size)) != nil {
		return 0, false
	}
	before := logosZgcRSSKB()
	var views [][]byte
	defer func() {
		for _, v := range views {
			syscall.Munmap(v)
		}
	}()
	for range 3 {
		v, err := syscall.Mmap(int(f.Fd()), 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err != nil {
			return 0, false
		}
		views = append(views, v)
		for off := 0; off < size; off += os.Getpagesize() {
			v[off] = 1
		}
	}
	return logosZgcRSSKB() - before, true
}
