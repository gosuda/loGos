// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package rt

// PR3 group parent-cancel regression. Parent cancel and parent deadline
// must wake a child held in netpoll-backed Read. Uses loopback TCP only,
// bounded waits, no ambient dials. Sync uses child-started channels, not
// scheduler sleeps; slot-reuse checks the received byte, not elapsed time.
//
// RED: on the head snapshot the held Read never wakes, so Wait hangs and
// the bounded selects below time out.

import (
	"context"
	"net"
	"testing"
	"time"
)

func pr3DialPair(t *testing.T) (cli, srv net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback unavailable: %v", err)
	}
	defer ln.Close()
	done := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			done <- c
		}
	}()
	cli, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Skipf("loopback dial unavailable: %v", err)
	}
	select {
	case srv = <-done:
	case <-time.After(2 * time.Second):
		cli.Close()
		t.Fatalf("accept timeout")
	}
	return cli, srv
}

func TestPR3GroupParentCancelWakesNetpoll(t *testing.T) {
	cli, srv := pr3DialPair(t)
	defer cli.Close()
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g, _ := NewGroup(ctx, 0)
	started := make(chan struct{})
	g.Go(func(context.Context) error {
		close(started)
		_, err := srv.Read(make([]byte, 1))
		return err
	})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		cli.Close()
		srv.Close()
		t.Fatalf("child did not reach Read")
	}
	cancel()
	done := make(chan error, 1)
	go func() { done <- g.Wait() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		cli.Close()
		srv.Close()
		t.Fatalf("Wait did not return within 2s after parent cancel")
	}
}

func TestPR3GroupParentDeadlineWakesNetpoll(t *testing.T) {
	cli, srv := pr3DialPair(t)
	defer cli.Close()
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	g, _ := NewGroup(ctx, 0)
	started := make(chan struct{})
	g.Go(func(context.Context) error {
		close(started)
		_, err := srv.Read(make([]byte, 1))
		return err
	})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		cli.Close()
		srv.Close()
		t.Fatalf("child did not reach Read")
	}
	done := make(chan error, 1)
	go func() { done <- g.Wait() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		cli.Close()
		srv.Close()
		t.Fatalf("Wait did not return within 2s after parent deadline")
	}
}

func TestPR3GroupSlotReuseNotSpurious(t *testing.T) {
	cli1, srv1 := pr3DialPair(t)
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	g1, _ := NewGroup(ctx1, 0)
	started1 := make(chan struct{})
	g1.Go(func(context.Context) error {
		close(started1)
		_, err := srv1.Read(make([]byte, 1))
		return err
	})
	select {
	case <-started1:
	case <-time.After(2 * time.Second):
		cli1.Close()
		srv1.Close()
		t.Fatalf("first child did not reach Read")
	}
	cancel1()
	done1 := make(chan error, 1)
	go func() { done1 <- g1.Wait() }()
	select {
	case <-done1:
	case <-time.After(2 * time.Second):
		cli1.Close()
		srv1.Close()
		t.Fatalf("first Wait did not return within 2s after parent cancel")
	}
	cli1.Close()
	srv1.Close()

	cli2, srv2 := pr3DialPair(t)
	defer cli2.Close()
	defer srv2.Close()
	g2, _ := NewGroup(context.Background(), 0)
	started2 := make(chan struct{})
	gotN := make(chan int, 1)
	gotByte := make(chan byte, 1)
	g2.Go(func(context.Context) error {
		close(started2)
		buf := make([]byte, 1)
		n, err := srv2.Read(buf)
		if err == nil {
			gotByte <- buf[0]
			gotN <- n
		} else {
			gotN <- -1
		}
		return err
	})
	select {
	case <-started2:
	case <-time.After(2 * time.Second):
		cli2.Close()
		srv2.Close()
		t.Fatalf("second child did not reach Read")
	}
	if _, err := cli2.Write([]byte{0x7A}); err != nil {
		cli2.Close()
		srv2.Close()
		t.Fatalf("controlled write failed: %v", err)
	}
	done2 := make(chan error, 1)
	go func() { done2 <- g2.Wait() }()
	select {
	case <-done2:
	case <-time.After(2 * time.Second):
		cli2.Close()
		srv2.Close()
		t.Fatalf("second Wait did not return within 2s")
	}
	select {
	case n := <-gotN:
		if n != 1 {
			t.Fatalf("reused slot waiter woke spuriously: n=%d, want 1", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("reader result missing")
	}
	select {
	case b := <-gotByte:
		if b != 0x7A {
			t.Fatalf("reused slot byte = %#x, want 0x7a", b)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("reader byte missing")
	}
}
