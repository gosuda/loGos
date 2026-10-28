// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !(goexperiment.logosgcmoving && (amd64 || arm64))

package runtime

// logosMovingMarkTermination is the mark-termination hook of the immix moving
// collector. The driver that runs the evacuation is built only with
// GOEXPERIMENT=logosgcmoving on amd64 and arm64; elsewhere, and with the
// experiment off, there is nothing to evacuate.
func logosMovingMarkTermination() {}
