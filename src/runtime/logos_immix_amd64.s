// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

#include "textflag.h"

// func logosHoleScanAVX2(lines *logosLineMark) (free [4]uint64)
TEXT ·logosHoleScanAVX2(SB), NOSPLIT, $0-40
	MOVQ	lines+0(FP), SI
	VPXOR	Y0, Y0, Y0
	VPCMPEQB	0(SI), Y0, Y1
	VPCMPEQB	32(SI), Y0, Y2
	VPCMPEQB	64(SI), Y0, Y3
	VPCMPEQB	96(SI), Y0, Y4
	VPCMPEQB	128(SI), Y0, Y5
	VPCMPEQB	160(SI), Y0, Y6
	VPCMPEQB	192(SI), Y0, Y7
	VPCMPEQB	224(SI), Y0, Y8
	VPMOVMSKB	Y1, AX
	VPMOVMSKB	Y2, BX
	VPMOVMSKB	Y3, CX
	VPMOVMSKB	Y4, DX
	VPMOVMSKB	Y5, R8
	VPMOVMSKB	Y6, R9
	VPMOVMSKB	Y7, R10
	VPMOVMSKB	Y8, R11
	VZEROUPPER
	SHLQ	$32, BX
	ORQ	BX, AX
	SHLQ	$32, DX
	ORQ	DX, CX
	SHLQ	$32, R9
	ORQ	R9, R8
	SHLQ	$32, R11
	ORQ	R11, R10
	MOVQ	AX, free_0+8(FP)
	MOVQ	CX, free_1+16(FP)
	MOVQ	R8, free_2+24(FP)
	MOVQ	R10, free_3+32(FP)
	RET
