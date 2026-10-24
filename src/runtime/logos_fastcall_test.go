// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"runtime"
	"testing"
	"unsafe"
)

// TestCgoFastcallPinSlots checks the fast-call record: enter writes the
// argument frame, the call-start stamp, and the flag; exit clears all three.
func TestCgoFastcallPinSlots(t *testing.T) {
	var probe int
	arg := unsafe.Pointer(&probe)
	slots, stamp, flag := runtime.LogosEnterFastCallForTest(arg, 12345)
	if !flag {
		t.Fatal("inFastCall not set by enter")
	}
	if want := [4]uintptr{uintptr(arg), 0, 0, 0}; slots != want {
		t.Fatalf("slots = %v, want %v", slots, want)
	}
	if stamp != 12345 {
		t.Fatalf("fastCallEnterNano = %d, want 12345", stamp)
	}

	slots, stamp, flag = runtime.LogosExitFastCallForTest()
	if flag {
		t.Fatal("inFastCall still set after exit")
	}
	if slots != [4]uintptr{} {
		t.Fatalf("slots = %v after exit, want zeros", slots)
	}
	if stamp != 0 {
		t.Fatalf("fastCallEnterNano = %d after exit, want 0", stamp)
	}
}
