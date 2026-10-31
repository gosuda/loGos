// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime

// Committed-range ledger for the zgc placement path.
//
// The ledger is a sorted, coalesced set of heap address ranges the heap
// has committed to the page allocator, added when the heap commits a
// range (sysAlloc grants it Reserved, grow commits it Prepared) and
// removed when the heap releases one (zgc source release; madvise forfeits
// the data but the mapping stays heap-owned, so the retire path re-adds
// the span range at free and post-retire reuse stays present). It is
// protected by the heap lock: every entry point asserts h.lock is held,
// takes no other lock, allocates nothing, and performs no write
// barriers, so it is safe on the system stack under h.lock and under
// the foreign gate split-phase in zgcReleasePage (which never holds
// h.lock and the gate at once).
//
// Debug gate: logosZgcCommitLedgerDebug is a GOEXPERIMENT-style constant.
// Normal builds keep it false, so every call site of the form
//
//	if logosZgcCommitLedgerDebug { ... }
//
// compiles away byte-for-byte and adds no per-allocation cost. Flip it
// to true in a debug build to enable the ledger and its invariants.
//
// Invariants (debug only):
//
//	I1 committed-grant: every address range the page allocator returns
//	(pageAlloc.alloc) lies fully inside a committed range. Violation
//	throws with the range, the call site category "pagealloc" (or the
//	grant site that published it: "low-window-grant", "exact-hit",
//	"sysalloc", "grow", "palloc-grow"), and whether the range was
//	absent from the ledger or only partially present.
//
//	I2 release-present: every release removes a range that is present in
//	the ledger. Violation throws with the range, the call site category
//	"release", and absent vs partially present.
//
// A throw therefore names the mechanism: an absent/partial grant is an
// uncommitted grant (fix commit-on-grant at the named transition); an
// absent/partial release is a release of memory the heap never committed
// or already released (fix the release ordering).
const logosZgcCommitLedgerDebug = false

const logosZgcCommitLedgerCap = 1024

type logosZgcCommitRange struct {
	base, size uintptr
}

var logosZgcCommitLedger [logosZgcCommitLedgerCap]logosZgcCommitRange
var logosZgcCommitLedgerN int

func logosZgcCommitLedgerEnd(e *logosZgcCommitRange) uintptr {
	return e.base + e.size
}

// logosZgcCommitLedgerAdd records [base, base+size) as committed,
// coalescing overlapping or adjacent entries. Caller holds h.lock.
func logosZgcCommitLedgerAdd(base, size uintptr) {
	if !logosZgcCommitLedgerDebug {
		return
	}
	assertLockHeld(&mheap_.lock)
	if size == 0 {
		return
	}
	end := base + size
	if end < base {
		print("logos zgc commit ledger: add [", hex(base), ", ", hex(end), ") wraps\n")
		throw("logos zgc commit ledger: add range wraps")
	}
	lo, hi := base, end
	i := 0
	for i < logosZgcCommitLedgerN && logosZgcCommitLedgerEnd(&logosZgcCommitLedger[i]) < lo {
		i++
	}
	for i < logosZgcCommitLedgerN && logosZgcCommitLedger[i].base <= hi {
		e := &logosZgcCommitLedger[i]
		if e.base < lo {
			lo = e.base
		}
		if ee := logosZgcCommitLedgerEnd(e); ee > hi {
			hi = ee
		}
		copy(logosZgcCommitLedger[i:], logosZgcCommitLedger[i+1:logosZgcCommitLedgerN])
		logosZgcCommitLedgerN--
	}
	if logosZgcCommitLedgerN >= logosZgcCommitLedgerCap {
		print("logos zgc commit ledger: full adding [", hex(lo), ", ", hex(hi), ")\n")
		throw("logos zgc commit ledger: full")
	}
	copy(logosZgcCommitLedger[i+1:], logosZgcCommitLedger[i:logosZgcCommitLedgerN])
	logosZgcCommitLedger[i] = logosZgcCommitRange{base: lo, size: hi - lo}
	logosZgcCommitLedgerN++
}

// logosZgcCommitLedgerAssertGrant throws unless [base, base+size) lies
// fully inside the committed set. Caller holds h.lock.
func logosZgcCommitLedgerAssertGrant(base, size uintptr, site string) {
	if !logosZgcCommitLedgerDebug {
		return
	}
	assertLockHeld(&mheap_.lock)
	end := base + size
	if size == 0 || end < base {
		print("logos zgc commit ledger: grant [", hex(base), ", ", hex(end), ") at ", site, " absent from committed ranges\n")
		throw("logos zgc commit ledger: grant absent from committed ranges")
	}
	for i := range logosZgcCommitLedgerN {
		e := &logosZgcCommitLedger[i]
		if base >= e.base && end <= logosZgcCommitLedgerEnd(e) {
			return
		}
	}
	for i := range logosZgcCommitLedgerN {
		e := &logosZgcCommitLedger[i]
		if base < logosZgcCommitLedgerEnd(e) && e.base < end {
			print("logos zgc commit ledger: grant [", hex(base), ", ", hex(end), ") at ", site, " partially present in committed ranges\n")
			throw("logos zgc commit ledger: grant partially present in committed ranges")
		}
	}
	print("logos zgc commit ledger: grant [", hex(base), ", ", hex(end), ") at ", site, " absent from committed ranges\n")
	throw("logos zgc commit ledger: grant absent from committed ranges")
}

// logosZgcCommitLedgerRemove asserts [base, base+size) is present and
// removes it, punching a hole in the covering entry. Caller holds h.lock.
func logosZgcCommitLedgerRemove(base, size uintptr, site string) {
	if !logosZgcCommitLedgerDebug {
		return
	}
	assertLockHeld(&mheap_.lock)
	end := base + size
	if size == 0 || end < base {
		print("logos zgc commit ledger: release [", hex(base), ", ", hex(end), ") at ", site, " absent from committed ranges\n")
		throw("logos zgc commit ledger: release absent from committed ranges")
	}
	for i := range logosZgcCommitLedgerN {
		e := &logosZgcCommitLedger[i]
		ee := logosZgcCommitLedgerEnd(e)
		if base < e.base || end > ee {
			continue
		}
		before := base - e.base
		afterBase := end
		after := ee - end
		switch {
		case before > 0 && after > 0:
			if logosZgcCommitLedgerN >= logosZgcCommitLedgerCap {
				print("logos zgc commit ledger: full splitting [", hex(base), ", ", hex(end), ") at ", site, "\n")
				throw("logos zgc commit ledger: full")
			}
			e.size = before
			copy(logosZgcCommitLedger[i+2:], logosZgcCommitLedger[i+1:logosZgcCommitLedgerN])
			logosZgcCommitLedger[i+1] = logosZgcCommitRange{base: afterBase, size: after}
			logosZgcCommitLedgerN++
		case before > 0:
			e.size = before
		case after > 0:
			e.base = afterBase
			e.size = after
		default:
			copy(logosZgcCommitLedger[i:], logosZgcCommitLedger[i+1:logosZgcCommitLedgerN])
			logosZgcCommitLedgerN--
		}
		return
	}
	for i := range logosZgcCommitLedgerN {
		e := &logosZgcCommitLedger[i]
		if base < logosZgcCommitLedgerEnd(e) && e.base < end {
			print("logos zgc commit ledger: release [", hex(base), ", ", hex(end), ") at ", site, " partially present in committed ranges\n")
			throw("logos zgc commit ledger: release partially present in committed ranges")
		}
	}
	print("logos zgc commit ledger: release [", hex(base), ", ", hex(end), ") at ", site, " absent from committed ranges\n")
	throw("logos zgc commit ledger: release absent from committed ranges")
}
