// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime

import "unsafe"

// logosPinSynctestBubble stops the bubble object from moving and returns
// the address the caller must store.
//
// A channel is allocated in a noscan span, so the heap walk never rewrites
// the bubble word it holds; logosPinChan already stops the channel itself
// from moving, but the bubble it names is an ordinary object that a moving
// collector would otherwise relocate. The goroutine's own g.bubble is
// reachable through the heap walk and is rewritten, so a moved bubble
// leaves the two naming different addresses and select.go reports a select
// on a synctest channel from outside the bubble that no test created.
// Holding the bubble in place keeps both words equal.
func logosPinSynctestBubble(b *synctestBubble) *synctestBubble {
	if b == nil {
		return nil
	}
	return (*synctestBubble)(logosPinSignalObject(unsafe.Pointer(b)))
}
