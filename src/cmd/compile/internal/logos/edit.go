// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package logos

import (
	"cmp"
	"fmt"
	"slices"
	"sort"
	"strings"

	"cmd/compile/internal/syntax"
)

// Edit replaces the source text from Start up to End with Text. Positions
// are the 1-based line and byte column of the original file; an edit with
// Start equal to End inserts Text.
type Edit struct {
	Start, End syntax.Pos
	Text       string
	// Chunks carry the origin of Text: nil means the whole text is
	// generated, non-nil chunks say which bytes are verbatim copies of
	// which original byte ranges (offsets into the original file).
	Chunks []chunk
}

// splice is an Edit resolved to byte offsets of one source text.
type splice struct {
	start, end int
	text       string
	// chunks carry the origin of text: nil means the whole text is
	// generated. applyChunks rebase-checks them like the offsets.
	chunks []chunk
	// owner is the wrapper ID of boundary inserts (heads, tails)
	// that a later wrap of the same wrapper drains into fused text;
	// zero for ordinary splices.
	owner int
	host  *cfHost
}

// lineStarts returns the byte offset at which each line of src begins.
func lineStarts(src []byte) []int {
	starts := []int{0}
	for i, c := range src {
		if c == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}

// offsetOf converts a position to a byte offset into a text of size bytes.
// Columns are byte columns of the position's own line; a column past the
// line end is rejected even when its offset lands inside a later line.
func offsetOf(starts []int, size int, pos syntax.Pos) (int, error) {
	return lineColOffset(starts, size, int(pos.Line()), int(pos.Col()))
}

// lineColOffset converts a 1-based line and byte column to a byte offset
// into a text of size bytes, with the checks of offsetOf.
func lineColOffset(starts []int, size, line, col int) (int, error) {
	if line < 1 || line > len(starts) || col < 1 {
		return 0, fmt.Errorf("position %d:%d is outside the source", line, col)
	}
	end := size
	if line < len(starts) {
		end = starts[line] - 1
	}
	off := starts[line-1] + col - 1
	if off > end {
		return 0, fmt.Errorf("position %d:%d is outside the source", line, col)
	}
	return off, nil
}

// resolve converts e to byte offsets and rejects an edit that belongs to
// another file or ends before it starts.
func resolve(src []byte, starts []int, base *syntax.PosBase, e Edit) (splice, error) {
	for _, pos := range []syntax.Pos{e.Start, e.End} {
		if pos.FileBase() != base {
			return splice{}, fmt.Errorf("edit position %v belongs to a different file than %s", pos, base.Filename())
		}
	}
	start, err := offsetOf(starts, len(src), e.Start)
	if err != nil {
		return splice{}, err
	}
	end, err := offsetOf(starts, len(src), e.End)
	if err != nil {
		return splice{}, err
	}
	if end < start {
		return splice{}, fmt.Errorf("edit at %v ends before it starts", e.Start)
	}
	return splice{start: start, end: end, text: e.Text, chunks: e.Chunks}, nil
}

// EditSpan records one applied edit in byte offsets: the bytes of the
// original text from Start up to End became Len bytes of the edited text.
// An insertion has Start equal to End. Spans come in source order and never
// overlap, so a reader can map offsets between the two texts.
type EditSpan struct {
	Start int `json:"start"`
	End   int `json:"end"`
	Len   int `json:"len"`
}

// ApplyEdits applies edits to src and returns the new text. base is the
// file base of the file whose positions the edits carry. Edits may be
// given in any order; they must not overlap. Insertions at one offset keep
// their input order.
func ApplyEdits(src []byte, base *syntax.PosBase, edits []Edit) ([]byte, error) {
	out, _, err := ApplyEditsMapped(src, base, edits)
	return out, err
}

// ApplyEditsMapped applies edits like ApplyEdits and also returns the
// applied edits as spans of byte offsets, in the order they took effect.
func ApplyEditsMapped(src []byte, base *syntax.PosBase, edits []Edit) ([]byte, []EditSpan, error) {
	out, spans, _, err := ApplyEditsSegments(src, base, edits)
	return out, spans, err
}

// ApplyEditsSegments applies edits like ApplyEdits and also returns the
// applied edits as spans and the origin segments of the whole output:
// every byte of the edited text is either a verbatim copy of an original
// byte range or generated, so a reader maps any output offset to its
// exact original offset or learns it has none.
func ApplyEditsSegments(src []byte, base *syntax.PosBase, edits []Edit) ([]byte, []EditSpan, []Segment, error) {
	want := base.Pos().FileBase()
	starts := lineStarts(src)
	splices := make([]splice, len(edits))
	for i, e := range edits {
		s, err := resolve(src, starts, want, e)
		if err != nil {
			return nil, nil, nil, err
		}
		splices[i] = s
	}
	text, err := applyChunks(otext{s: string(src)}, splices)
	if err != nil {
		return nil, nil, nil, err
	}
	text.checkChunks(src)
	out := []byte(text.s)
	return out, spansOf(splices), segmentsOf(splices, len(out)), nil
}

// spansOf converts splices, already sorted by applySplices, to spans.
func spansOf(sorted []splice) []EditSpan {
	spans := make([]EditSpan, len(sorted))
	for i, s := range sorted {
		spans[i] = EditSpan{Start: s.start, End: s.end, Len: len(s.text)}
	}
	return spans
}

// sortSplices orders splices by start, then end, keeping the input order
// of equal splices.
func sortSplices(splices []splice) {
	slices.SortStableFunc(splices, func(a, b splice) int {
		return cmp.Or(cmp.Compare(a.start, b.start), cmp.Compare(a.end, b.end))
	})
}

// applySplices applies byte-offset splices to src. It sorts splices in
// place and reports an error when two of them overlap.
func applySplices(src []byte, splices []splice) ([]byte, error) {
	t, err := applyChunks(otext{s: string(src)}, splices)
	return []byte(t.s), err
}

// applyChunks applies byte-offset splices to src and returns the
// composed text with origin chunks: gap bytes inherit src's mapping and
// splice text carries its own. The bytes of the result equal
// applySplices'; only the origin is added. Splices are sorted in place;
// overlapping splices are an error.
func applyChunks(src otext, splices []splice) (otext, error) {
	sortSplices(splices)
	var sb strings.Builder
	var chunks []chunk
	out := 0
	prev := 0
	gap := func(upto int) {
		if upto <= prev {
			return
		}
		sb.WriteString(src.s[prev:upto])
		for _, c := range src.slice(prev, upto).chunks {
			chunks = append(chunks, chunk{out: out + c.out, orig: c.orig, n: c.n})
		}
		out += upto - prev
	}
	for _, s := range splices {
		if s.start < prev {
			return otext{}, fmt.Errorf("overlapping edits at byte offset %d", s.start)
		}
		gap(s.start)
		sb.WriteString(s.text)
		for _, c := range s.chunks {
			if c.n == 0 {
				continue
			}
			chunks = append(chunks, chunk{out: out + c.out, orig: c.orig, n: c.n})
		}
		out += len(s.text)
		prev = s.end
	}
	gap(len(src.s))
	res := otext{s: sb.String(), chunks: chunks}
	if res.s == "" {
		return otext{}, nil
	}
	return res, nil
}

// editsOf converts splices of one file to Edits positioned on base.
func editsOf(src []byte, base *syntax.PosBase, splices []splice) []Edit {
	starts := lineStarts(src)
	posAt := func(off int) syntax.Pos {
		line := sort.SearchInts(starts, off+1) - 1
		return syntax.MakePos(base, uint(line+1), uint(off-starts[line]+1))
	}
	sorted := slices.Clone(splices)
	sortSplices(sorted)
	edits := make([]Edit, len(sorted))
	for i, s := range sorted {
		edits[i] = Edit{Start: posAt(s.start), End: posAt(s.end), Text: s.text, Chunks: s.chunks}
	}
	return edits
}
