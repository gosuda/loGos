// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package wasm

import (
	"cmd/internal/obj/wasm"
	"cmd/link/internal/ld"
	"cmd/link/internal/loader"
)

// The module parts of the stack-switching mode (loGo/s M16-S1,
// --wasm-features=stack-switching; cmd/internal/obj/wasm/stackswitch_logos.go).
// A runtime assembled for the mode defines wasm_cont_entry, the body of
// every goroutine continuation; when the link reaches it, the module gets
// the type func() at wasm.ContFuncType, the type cont func() at
// wasm.ContType, table wasm.ContTable of (ref null cont), and tag
// wasm.ContTag of type func(). The instructions name them by these fixed
// indices.

const sectionTag = 13

// wasm_cont_run and wasm_cont_entry take and return nothing: the root loop
// calls the first, and the second is the function of every continuation.
func init() {
	wasmFuncTypes["wasm_cont_run"] = &wasmFuncType{Params: []byte{}}
	wasmFuncTypes["wasm_cont_entry"] = &wasmFuncType{Params: []byte{}}
}

// stackSwitch reports whether the module being written uses the mode.
var stackSwitch bool

// logosStackSwitchTypes sets stackSwitch and appends the two types of the
// mode after the type of normal Go functions.
func logosStackSwitchTypes(ldr *loader.Loader, types []*wasmFuncType) []*wasmFuncType {
	s := ldr.Lookup("wasm_cont_entry", 0)
	stackSwitch = s != 0 && ldr.AttrReachable(s)
	if !stackSwitch {
		return types
	}
	if len(types) != wasm.ContFuncType {
		panic("wasm: stack-switching types are not at their fixed indices")
	}
	return append(types, &wasmFuncType{Params: []byte{}, Results: []byte{}}, &wasmFuncType{cont: true})
}

// logosWriteContType writes the continuation type: cont ContFuncType.
func logosWriteContType(ctxt *ld.Link) {
	ctxt.Out.WriteByte(0x5d)
	writeUleb128(ctxt.Out, wasm.ContFuncType)
}

// logosContTables returns the number of tables the mode adds.
func logosContTables() uint64 {
	if stackSwitch {
		return 1
	}
	return 0
}

// logosWriteContTable writes the continuation table: (ref null ContType),
// no maximum, initially empty; the runtime grows it.
func logosWriteContTable(ctxt *ld.Link) {
	if !stackSwitch {
		return
	}
	ctxt.Out.WriteByte(0x63)
	writeUleb128(ctxt.Out, wasm.ContType)
	ctxt.Out.WriteByte(0x00)
	writeUleb128(ctxt.Out, 0)
}

// logosWriteTagSec writes the tag section, which sits between the memory
// and the global sections: one tag of type ContFuncType.
func logosWriteTagSec(ctxt *ld.Link) {
	if !stackSwitch {
		return
	}
	sizeOffset := writeSecHeader(ctxt, sectionTag)
	writeUleb128(ctxt.Out, 1)
	ctxt.Out.WriteByte(0x00) // exception attribute
	writeUleb128(ctxt.Out, wasm.ContFuncType)
	writeSecSize(ctxt, sizeOffset)
}
