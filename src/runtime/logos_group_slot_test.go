// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build unix || (js && wasm) || wasip1 || windows

package runtime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"logos/rt"
)

// The group tag of a goroutine is a slot index the runtime lends to one
// logos/rt.Group at a time (SPEC Sec 2.6, T-206). These tests pin the
// lifetime the slot bookkeeping buys, through public Groups and
// blocking reads: a slot stays with its group until the join, so the
// failure of another group never wakes the joining group's blocked
// child; and a slot lent out again after a failure starts clean, so the
// next group's child blocks until data arrives instead of timing out
// spuriously.

// A group whose Wait is still joining keeps its slot: groups created
// meanwhile get other slots, so their failures never wake the joining
// group's blocked child.
func TestLogosGroupNetpollSlotHeldUntilJoin(t *testing.T) {
	joinClient, joinServer := logosConnPair(t)
	joining, _ := rt.NewGroup(t.Context(), 0)
	started := make(chan struct{})
	joiningRead := make(chan error, 1)
	joining.Go(func(ctx context.Context) error {
		close(started)
		buf := make([]byte, 1)
		_, err := joinServer.Read(buf)
		joiningRead <- err
		return nil
	})
	joinDone := make(chan error, 1)
	go func() { joinDone <- joining.Wait() }()
	<-started
	// Let the child park in the netpoll wait before other groups fail.
	time.Sleep(100 * time.Millisecond)

	const failing = 8
	var groups [failing]*rt.Group
	for i := range groups {
		groups[i], _ = rt.NewGroup(t.Context(), 0)
		groups[i].Go(func(ctx context.Context) error { return errLogosSibling })
	}
	for _, g := range groups {
		if err := logosGroupWait(t, g, 5*time.Second); !errors.Is(err, errLogosSibling) {
			t.Fatalf("Wait = %v, want %v", err, errLogosSibling)
		}
	}

	select {
	case err := <-joiningRead:
		t.Fatalf("joining group's Read returned %v after another group failed; want still blocked", err)
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := joinClient.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := <-joinDone; err != nil {
		t.Fatalf("joining group Wait = %v, want nil", err)
	}
	if err := <-joiningRead; err != nil {
		t.Fatalf("joining group's Read = %v after data arrived, want nil", err)
	}
}

// A slot lent out again after its group's failure starts clean: the next
// group's child blocks until data arrives instead of timing out from the
// old failure.
func TestLogosGroupSlotReusedAfterJoinStartsClean(t *testing.T) {
	failed, _ := rt.NewGroup(t.Context(), 0)
	failed.Go(func(ctx context.Context) error { return errLogosSibling })
	if err := logosGroupWait(t, failed, 5*time.Second); !errors.Is(err, errLogosSibling) {
		t.Fatalf("Wait = %v, want %v", err, errLogosSibling)
	}

	client, server := logosConnPair(t)
	next, _ := rt.NewGroup(t.Context(), 0)
	started := make(chan struct{})
	readDone := make(chan error, 1)
	next.Go(func(ctx context.Context) error {
		close(started)
		buf := make([]byte, 1)
		_, err := server.Read(buf)
		readDone <- err
		return nil
	})
	<-started
	// Let the child park. A stale cancellation from the released slot
	// would wake it promptly with a timeout; still-blocked proves clean reuse.
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-readDone:
		t.Fatalf("reused slot's Read returned %v without data; want still blocked", err)
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := client.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatalf("reused slot's Read = %v after data arrived, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reused slot's Read still blocked after data arrived")
	}
	if err := logosGroupWait(t, next, 5*time.Second); err != nil {
		t.Fatalf("next group Wait = %v, want nil", err)
	}
}
