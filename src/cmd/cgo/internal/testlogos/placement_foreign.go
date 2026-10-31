// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package testlogos

/*
extern int sum_bytes(unsigned char *p, int n);

#cgo noescape sum_bytes
#cgo nocallback sum_bytes
*/
import "C"

import "runtime"

type placementBytes = C.uchar

var placementCapacity = 8 + runtime.NumCPU()%8

func placementPatternFill(buf []placementBytes) {
	for i := range buf {
		buf[i] = C.uchar((i*7 + 11) & 0xff)
	}
}

func placementChecksumDynamic() int {
	buf := make([]placementBytes, placementCapacity)
	placementPatternFill(buf)
	return int(C.sum_bytes(&buf[0], C.int(len(buf))))
}

func placementControl(buf []placementBytes) func() int {
	return func() int {
		placementPatternFill(buf)
		return int(C.sum_bytes(&buf[0], C.int(len(buf))))
	}
}
