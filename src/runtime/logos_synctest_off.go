// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !goexperiment.logosgcmoving || !(amd64 || arm64)

package runtime

// logosPinSynctestBubble is the identity when no moving collector can run:
// the bubble never moves, so the caller's pointer stays valid.
func logosPinSynctestBubble(b *synctestBubble) *synctestBubble {
	return b
}
