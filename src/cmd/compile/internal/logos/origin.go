// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package logos

import (
	"strings"

	"cmd/compile/internal/base"
)

// This file carries the origin of eject text: which bytes are verbatim
// copies of the original source and which the compiler generated. Every
// composed text is an otext; its chunks say, per byte range, where the
// bytes came from. Chunks are the only source of provenance: a byte not
// covered by a chunk is generated, and a covered byte maps back to its
// original offset exactly. The segments of an edit map are built from
// the chunks of the applied splices, so a reader can tell a generated
// finding (dropped) from a finding on user-written tokens inside a
// rewritten span (mapped to the exact original offset).

// chunk records that bytes [Out, Out+N) of a composed text are a
// verbatim copy of bytes [Orig, Orig+N) of the original file.
type chunk struct{ out, orig, n int }

// otext is eject text plus its origin chunks. Chunks are ordered by
// Out, disjoint, and need not cover the text: uncovered bytes are
// generated. A nil chunk list on a non-empty text means fully
// generated.
type otext struct {
	s      string
	chunks []chunk
}

// gen returns generated text.
func gen(s string) otext { return otext{s: s} }

// cat concatenates parts, rebasing their chunks into the result.
func cat(parts ...otext) otext {
	if len(parts) == 1 {
		return parts[0]
	}
	var sb strings.Builder
	for _, p := range parts {
		sb.WriteString(p.s)
	}
	out := sb.String()
	if out == "" {
		return otext{}
	}
	var chunks []chunk
	off := 0
	for _, p := range parts {
		for _, c := range p.chunks {
			if c.n == 0 {
				continue
			}
			chunks = append(chunks, chunk{out: off + c.out, orig: c.orig, n: c.n})
		}
		off += len(p.s)
	}
	return otext{s: out, chunks: chunks}
}

// slice returns t[start:end] with the chunks clipped to the slice and
// rebased to its start.
func (t otext) slice(start, end int) otext {
	sub := t.s[start:end]
	if sub == "" {
		return otext{}
	}
	var chunks []chunk
	for _, c := range t.chunks {
		cs, ce := c.out, c.out+c.n
		if ce <= start || cs >= end {
			continue
		}
		if cs < start {
			c.orig += start - cs
			cs = start
		}
		if ce > end {
			ce = end
		}
		chunks = append(chunks, chunk{out: cs - start, orig: c.orig, n: ce - cs})
	}
	return otext{s: sub, chunks: chunks}
}

// trim returns t with ASCII white space removed from both ends; the
// chunk edges follow the removed bytes.
func (t otext) trim() otext {
	start, end := 0, len(t.s)
	for start < end && isSpace(t.s[start]) {
		start++
	}
	for end > start && isSpace(t.s[end-1]) {
		end--
	}
	return t.slice(start, end)
}

// trimRight returns t with the bytes of cutset removed from the end;
// the chunk edges follow the removed bytes.
func (t otext) trimRight(cutset string) otext {
	end := len(t.s)
	for end > 0 && strings.IndexByte(cutset, t.s[end-1]) >= 0 {
		end--
	}
	return t.slice(0, end)
}

// checkChunks verifies that the chunks of t are ordered, disjoint, in
// range, and — against orig — byte-exact copies of the original file.
// A violation is a producer bug; it stops the compiler rather than
// letting a wrong origin reach an edit map.
func (t *otext) checkChunks(orig []byte) {
	prevEnd := -1
	for i, c := range t.chunks {
		if c.n <= 0 || c.out < 0 || c.orig < 0 || c.out+c.n > len(t.s) || c.orig+c.n > len(orig) {
			base.Fatalf("logos: invalid origin chunk %d %+v for %d bytes of text", i, c, len(t.s))
		}
		if c.out < prevEnd {
			base.Fatalf("logos: overlapping origin chunks at %d in %q", c.out, t.s)
		}
		if t.s[c.out:c.out+c.n] != string(orig[c.orig:c.orig+c.n]) {
			base.Fatalf("logos: origin chunk %+v is not a verbatim copy", c)
		}
		prevEnd = c.out + c.n
	}
}

// substNul replaces every NUL-delimited placeholder of t through lookup.
// Placeholders are the marker names of renderText and the move marks of
// resolveChainMoves; a name lookup does not know stays verbatim, which
// keeps the per-mark ReplaceAll semantics of the move resolution. Chunks
// after a substitution shift with the text.
func substNul(t otext, lookup func(mark string) (otext, bool)) otext {
	if !strings.ContainsRune(t.s, 0) {
		return t
	}
	var sb strings.Builder
	var chunks []chunk
	prev := 0
	scan := 0
	out := 0
	flush := func(upto int) {
		if upto <= prev {
			return
		}
		sb.WriteString(t.s[prev:upto])
		for _, c := range t.slice(prev, upto).chunks {
			chunks = append(chunks, chunk{out: out + c.out, orig: c.orig, n: c.n})
		}
		out += upto - prev
	}
	for scan < len(t.s) {
		i := strings.IndexByte(t.s[scan:], 0)
		if i < 0 {
			break
		}
		i += scan
		j := strings.IndexByte(t.s[i+1:], 0)
		if j < 0 {
			break
		}
		j += i + 1
		if repl, ok := lookup(t.s[i : j+1]); ok {
			flush(i)
			sb.WriteString(repl.s)
			for _, c := range repl.chunks {
				chunks = append(chunks, chunk{out: out + c.out, orig: c.orig, n: c.n})
			}
			out += len(repl.s)
			prev = j + 1
		}
		scan = j + 1
	}
	flush(len(t.s))
	res := otext{s: sb.String(), chunks: chunks}
	if res.s == "" {
		return otext{}
	}
	return res
}

// substNeedle replaces every occurrence of needle with repl, shifting
// and splitting chunks across the occurrence. The needle may sit inside
// a user chunk; the bytes around it keep their origin and the
// replacement is generated unless repl carries its own.
func substNeedle(t otext, needle, repl otext) otext {
	if needle.s == "" || !strings.Contains(t.s, needle.s) {
		return t
	}
	var sb strings.Builder
	var chunks []chunk
	prev := 0
	out := 0
	emit := func(p otext) {
		sb.WriteString(p.s)
		for _, c := range p.chunks {
			chunks = append(chunks, chunk{out: out + c.out, orig: c.orig, n: c.n})
		}
		out += len(p.s)
	}
	for {
		i := strings.Index(t.s[prev:], needle.s)
		if i < 0 {
			break
		}
		i += prev
		emit(t.slice(prev, i))
		emit(repl)
		prev = i + len(needle.s)
	}
	emit(t.slice(prev, len(t.s)))
	res := otext{s: sb.String(), chunks: chunks}
	if res.s == "" {
		return otext{}
	}
	return res
}

// srcBuf is a byte buffer with a piecewise origin mapping: chunks map
// buffer offsets to original-file offsets. fileLower keeps one over its
// growing source buffer: the original bytes map to themselves and every
// adopted region maps its bytes to the original ranges the region was
// copied from. Composition never uses a scalar base; it slices this
// buffer, so a copy assembled from several original ranges keeps each
// range's true origin.
type srcBuf struct {
	b      []byte
	chunks []chunk
}

// origBuf returns the identity mapping of b.
func origBuf(b []byte) srcBuf {
	if len(b) == 0 {
		return srcBuf{}
	}
	return srcBuf{b: b, chunks: []chunk{{out: 0, orig: 0, n: len(b)}}}
}

// sliceO returns the otext of b[start:end] with the mapping clipped to
// the range.
func (b *srcBuf) sliceO(start, end int) otext {
	return otext{s: string(b.b[start:end]), chunks: b.clip(start, end)}
}

// clip returns the chunks covering [start, end), rebased to start.
func (b *srcBuf) clip(start, end int) []chunk {
	if end <= start {
		return nil
	}
	var out []chunk
	for _, c := range b.chunks {
		cs, ce := c.out, c.out+c.n
		if ce <= start || cs >= end {
			continue
		}
		oc := c.orig
		if cs < start {
			oc += start - cs
			cs = start
		}
		if ce > end {
			ce = end
		}
		out = append(out, chunk{out: cs - start, orig: oc, n: ce - cs})
	}
	return out
}

// appendO appends the bytes of t and its chunks (rebased by the append
// position) to the buffer. The caller owns resyncing b with the byte
// slice it grows from.
func (b *srcBuf) appendO(t otext) {
	base := len(b.b)
	b.b = append(b.b, t.s...)
	for _, c := range t.chunks {
		if c.n == 0 {
			continue
		}
		b.chunks = append(b.chunks, chunk{out: base + c.out, orig: c.orig, n: c.n})
	}
}
