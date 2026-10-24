// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && linux && (amd64 || arm64)

package runtime

// LogosZgcReadZero reads n bytes of /dev/zero into addr through the
// kernel and returns the byte count or the negated errno.
func LogosZgcReadZero(addr uintptr, n int32) int32 {
	path := []byte("/dev/zero\x00")
	fd := open(&path[0], 0, 0)
	if fd < 0 {
		return fd
	}
	defer closefd(fd)
	return read(fd, add(nil, addr), n)
}
