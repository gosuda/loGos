// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package lsp

import (
	"bytes"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// questionMap lowers aLgo with the fake lowering and returns the texts and
// the position map between them.
func questionMap(t *testing.T) (src, out string, pm *posMap) {
	t.Helper()
	lowered, m, ok := lowerQuestion([]byte(aLgo))
	if !ok {
		t.Fatal("aLgo does not lower")
	}
	m.Source = "a.lgo"
	pm, err := newPosMap([]byte(aLgo), lowered, m)
	if err != nil {
		t.Fatal(err)
	}
	return aLgo, string(lowered), pm
}

// segBuilder builds the Segments of one lowered text in order: copy
// appends n lowered bytes copied from the original offset orig, gen
// appends n generated lowered bytes, and adjacent runs of one kind merge,
// so a deletion (which appends nothing) fuses the copies around it.
type segBuilder struct {
	pos  int
	segs []Segment
}

// copy appends a run of n lowered bytes copied from the original offset orig.
func (b *segBuilder) copy(orig, n int) {
	if n <= 0 {
		return
	}
	if k := len(b.segs); k > 0 {
		if last := &b.segs[k-1]; last.Orig >= 0 && last.Orig+last.Len == orig && last.Out+last.Len == b.pos {
			last.Len += n
			b.pos += n
			return
		}
	}
	b.segs = append(b.segs, Segment{Out: b.pos, Len: n, Orig: orig})
	b.pos += n
}

// gen appends n generated lowered bytes.
func (b *segBuilder) gen(n int) {
	if n <= 0 {
		return
	}
	if k := len(b.segs); k > 0 {
		if last := &b.segs[k-1]; last.Orig == -1 && last.Out+last.Len == b.pos {
			last.Len += n
			b.pos += n
			return
		}
	}
	b.segs = append(b.segs, Segment{Out: b.pos, Len: n, Orig: -1})
	b.pos += n
}

// done returns the built segments.
func (b *segBuilder) done() []Segment {
	return b.segs
}

// spanSegs builds v2 segments for a lowering described by edit spans
// alone: the bytes between edits are copied at identical offsets, and each
// edit is one generated run of its replacement length. An insertion
// (including an EOF trailer) becomes a generated run; a deletion appends
// nothing and fuses the copies around it.
func spanSegs(srcLen int, edits []EditSpan) []Segment {
	var b segBuilder
	prev := 0
	for _, s := range edits {
		b.copy(prev, s.Start-prev)
		b.gen(s.Len)
		prev = s.End
	}
	b.copy(prev, srcLen-prev)
	return b.done()
}

// TestPosMapKeepsOperandOfQuestionStatement checks that the operand a ?
// statement copies into its expansion maps exactly, both ways, so
// hover and go-to-definition on `os.ReadFile(path)` hit the same tokens
// in the source and in the lowered text.
func TestPosMapKeepsOperandOfQuestionStatement(t *testing.T) {
	src, out, pm := questionMap(t)
	srcLine := lineOf(src, 5)
	for _, word := range []string{"data", "os", "ReadFile", "path"} {
		orig := Position{Line: 5, Character: strings.Index(srcLine, word)}
		low := pm.toLowered(orig)
		if got := lineOf(out, low.Line)[low.Character:]; !strings.HasPrefix(got, word) {
			t.Errorf("%s: source %v maps to lowered %v, which starts %q", word, orig, low, got)
		}
		if back := pm.toOriginal(low); back != orig {
			t.Errorf("%s: round trip %v -> %v -> %v", word, orig, low, back)
		}
	}
}

// TestPosMapGeneratedLinesMapToQuestionStatement checks that every
// position inside the three lines the ? statement expands to maps back
// into the statement of the source: the first line to its text, the
// generated lines to the ? token that produced them.
func TestPosMapGeneratedLinesMapToQuestionStatement(t *testing.T) {
	src, out, pm := questionMap(t)
	srcLine := lineOf(src, 5)
	qcol := strings.Index(srcLine, "?")
	if qcol < 0 {
		t.Fatal("no ? in the source line")
	}
	for line := 5; line <= 7; line++ {
		text := lineOf(out, line)
		for col := 0; col <= len(text); col++ {
			orig := pm.toOriginal(Position{Line: line, Character: col})
			if orig.Line != 5 || orig.Character > len(srcLine) {
				t.Errorf("lowered %d:%d maps to %v, outside source line 5", line, col, orig)
			}
			if line > 5 && orig.Character != qcol && orig.Character != qcol+1 {
				t.Errorf("lowered generated %d:%d maps to column %d, want the ? at %d", line, col, orig.Character, qcol)
			}
		}
	}
	q := Position{Line: 5, Character: qcol}
	low := pm.toLowered(q)
	if low.Line != 5 || low.Character != len(lineOf(out, 5)) {
		t.Errorf("? at %v maps to %v, want the end of lowered line 5", q, low)
	}
	rng := pm.rangeToOriginal(Range{Start: Position{Line: 6, Character: 0}, End: Position{Line: 7, Character: len(lineOf(out, 7))}})
	want := Range{Start: q, End: shift(q, 1)}
	if rng != want {
		t.Errorf("range over the generated lines maps to %v, want the ? token %v", rng, want)
	}
}

// TestPosMapRangeToLoweredCoversQuestionStatement checks that a source
// range over the whole statement maps to a range that covers the lowered
// statement, the call first.
func TestPosMapRangeToLoweredCoversQuestionStatement(t *testing.T) {
	src, out, pm := questionMap(t)
	srcLine := lineOf(src, 5)
	r := Range{
		Start: Position{Line: 5, Character: strings.Index(srcLine, "data")},
		End:   Position{Line: 5, Character: len(srcLine)},
	}
	got := pm.rangeToLowered(r)
	if got.Start != (Position{Line: 5, Character: 1}) || got.End != (Position{Line: 7, Character: len(lineOf(out, 7))}) {
		t.Errorf("range %v maps to %v, want 5:1 to the end of lowered line 7", r, got)
	}
}

// TestPosMapInsertionAtEndOfFile checks the edge of text the lowering
// appends: the end of the source maps before the appended text on the
// trailing edge and after it on the leading edge.
func TestPosMapInsertionAtEndOfFile(t *testing.T) {
	_, out, pm := questionMap(t)
	end := len(aLgo)
	if got, ok := pm.lowOffset(end, trailing); !ok || got != len(out)-len(siteTrailer) {
		t.Errorf("trailing end maps to (%d, %v), want (%d, true)", got, ok, len(out)-len(siteTrailer))
	}
	if got, ok := pm.lowOffset(end, leading); !ok || got != len(out) {
		t.Errorf("leading end maps to (%d, %v), want (%d, true)", got, ok, len(out))
	}
	if got, gen := pm.origOffset(len(out), leading); gen || got != end {
		t.Errorf("end of lowered text maps to (%d, %v), want (%d, false)", got, gen, end)
	}
}

// TestPosMapDeletion checks both edges at the seam of deleted text.
func TestPosMapDeletion(t *testing.T) {
	const deleted = "import \"comptime\"\n"
	src := []byte(deleted + "x := 1\n")
	out := []byte("x := 1\n")
	edits := []EditSpan{{Start: 0, End: len(deleted), Len: 0}}
	pm, err := newPosMap(src, out, EditMap{
		Version: EditMapVersion, Source: "d.lgo", Size: len(src), Out: len(out),
		Edits: edits, Segments: spanSegs(len(src), edits),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, gen := pm.origOffset(0, leading); gen || got != len(deleted) {
		t.Errorf("leading seam maps to (%d, %v), want (%d, false) after the deleted line", got, gen, len(deleted))
	}
	if got, gen := pm.origOffset(0, trailing); gen || got != 0 {
		t.Errorf("trailing seam maps to (%d, %v), want (0, false) before the deleted line", got, gen)
	}
	if got := pm.toLowered(Position{Line: 1, Character: 0}); got != (Position{Line: 0, Character: 0}) {
		t.Errorf("the line after the deletion maps to %v, want 0:0", got)
	}
}

// TestNewPosMapRejectsBadMaps checks that a map that cannot describe its
// texts is refused, so a stale or corrupt sidecar never maps positions.
func TestNewPosMapRejectsBadMaps(t *testing.T) {
	src, out := []byte("abc"), []byte("aXXc")
	goodSegs := spanSegs(len(src), []EditSpan{{Start: 1, End: 2, Len: 2}})
	good := EditMap{Version: EditMapVersion, Source: "x.lgo", Size: 3, Out: 4, Edits: []EditSpan{{Start: 1, End: 2, Len: 2}}, Segments: goodSegs}
	if _, err := newPosMap(src, out, good); err != nil {
		t.Fatalf("good map rejected: %v", err)
	}
	bad := func(size, outSize int, edits ...EditSpan) EditMap {
		return EditMap{Version: EditMapVersion, Source: "x.lgo", Size: size, Out: outSize, Edits: edits, Segments: goodSegs}
	}
	cases := map[string]EditMap{
		"version":      {Version: 1, Source: "x.lgo", Size: 3, Out: 4, Edits: good.Edits, Segments: goodSegs},
		"source size":  bad(4, 4, good.Edits...),
		"out size":     bad(3, 5, good.Edits...),
		"sum":          bad(3, 4, EditSpan{Start: 1, End: 2, Len: 3}),
		"past the end": bad(3, 4, EditSpan{Start: 1, End: 9, Len: 10}),
		"inverted":     bad(3, 4, EditSpan{Start: 2, End: 1, Len: 3}),
		"negative len": bad(3, 4, EditSpan{Start: 1, End: 2, Len: -1}, EditSpan{Start: 2, End: 2, Len: 2}),
		"overlap":      bad(3, 5, EditSpan{Start: 0, End: 2, Len: 2}, EditSpan{Start: 1, End: 2, Len: 2}),
		"out of order": bad(3, 4, EditSpan{Start: 2, End: 3, Len: 1}, EditSpan{Start: 0, End: 1, Len: 1}),
		"no segments": {
			Version: EditMapVersion, Source: "x.lgo", Size: 3, Out: 4,
			Edits: good.Edits,
		},
		"segments short": {
			Version: EditMapVersion, Source: "x.lgo", Size: 3, Out: 4,
			Edits:    good.Edits,
			Segments: []Segment{{Out: 0, Len: 3, Orig: 0}},
		},
		"segments overlap": {
			Version: EditMapVersion, Source: "x.lgo", Size: 3, Out: 4,
			Edits:    good.Edits,
			Segments: []Segment{{Out: 0, Len: 2, Orig: -1}, {Out: 1, Len: 3, Orig: -1}},
		},
		"copy outside source": {
			Version: EditMapVersion, Source: "x.lgo", Size: 3, Out: 4,
			Edits:    good.Edits,
			Segments: []Segment{{Out: 0, Len: 4, Orig: 3}},
		},
		"copy bytes differ": {
			Version: EditMapVersion, Source: "x.lgo", Size: 3, Out: 4,
			Edits:    good.Edits,
			Segments: []Segment{{Out: 0, Len: 1, Orig: 0}, {Out: 1, Len: 2, Orig: -1}, {Out: 3, Len: 1, Orig: 0}},
		},
	}
	for name, m := range cases {
		if _, err := newPosMap(src, out, m); err == nil {
			t.Errorf("%s: map %+v accepted", name, m)
		}
	}
}

// TestPosMapSegmentsMapCopiedRunsExactly checks that every lowered byte a
// segment marks as copied — the variable name and the operand the fake
// lowering reuses inside a replaced ? statement — maps exactly in both
// directions. The leading edge is exact on every copied byte; the trailing
// edge is exact except at the very start of a run, where it still sits
// before text the lowering inserted there.
func TestPosMapSegmentsMapCopiedRunsExactly(t *testing.T) {
	_, _, pm := questionMap(t)
	copied := 0
	for _, s := range pm.segs {
		if s.Orig < 0 {
			continue
		}
		copied += s.Len
		for i := 0; i < s.Len; i++ {
			if got, ok := pm.lowOffset(s.Orig+i, leading); !ok || got != s.Out+i {
				t.Errorf("copied original %d maps to (%d, %v), want (%d, true)", s.Orig+i, got, ok, s.Out+i)
			}
			if i > 0 {
				if got, ok := pm.lowOffset(s.Orig+i, trailing); !ok || got != s.Out+i {
					t.Errorf("copied original %d trails to (%d, %v), want (%d, true)", s.Orig+i, got, ok, s.Out+i)
				}
			}
			if back, gen := pm.origOffset(s.Out+i, leading); gen || back != s.Orig+i {
				t.Errorf("copied lowered %d maps to (%d, %v), want (%d, false)", s.Out+i, back, gen, s.Orig+i)
			}
		}
	}
	if copied == 0 {
		t.Error("the ? lowering carries no copied bytes")
	}
}

// TestPosMapLargeReplacementMapsBySegments checks that a replaced span far
// beyond the old refinement table bound still maps exactly: segments carry
// the origin, so there is no table to overflow and no cell limit. Deleted
// original bytes carry no exact origin, and generated bytes anchor to the
// copies around them.
func TestPosMapLargeReplacementMapsBySegments(t *testing.T) {
	src := []byte("w" + strings.Repeat("(", 2048))
	mid := src[512:1536]
	out := append(append([]byte("gen<"), mid...), []byte(">gen")...)
	edits := []EditSpan{{Start: 0, End: len(src), Len: len(out)}}
	m := EditMap{
		Version: EditMapVersion, Source: "big.lgo", Size: len(src), Out: len(out),
		Edits: edits,
		Segments: []Segment{
			{Out: 0, Len: 4, Orig: -1},
			{Out: 4, Len: len(mid), Orig: 512},
			{Out: 4 + len(mid), Len: 4, Orig: -1},
		},
	}
	pm, err := newPosMap(src, out, m)
	if err != nil {
		t.Fatal(err)
	}
	for i := range mid {
		if got, ok := pm.lowOffset(512+i, leading); !ok || got != 4+i {
			t.Fatalf("copied original %d maps to (%d, %v), want (%d, true)", 512+i, got, ok, 4+i)
		}
		if back, gen := pm.origOffset(4+i, leading); gen || back != 512+i {
			t.Fatalf("copied lowered %d maps to (%d, %v), want (%d, false)", 4+i, back, gen, 512+i)
		}
	}
	if got, ok := pm.lowOffset(0, leading); ok || got != 0 {
		t.Errorf("deleted original 0 maps to (%d, %v), want (0, false)", got, ok)
	}
	if got, gen := pm.origOffset(1, leading); !gen || got != 0 {
		t.Errorf("generated prefix maps to (%d, %v), want (0, true)", got, gen)
	}
	if got, gen := pm.origOffset(len(out)-2, leading); !gen || got != 1536 {
		t.Errorf("generated suffix maps to (%d, %v), want (1536, true)", got, gen)
	}
}

// assertSpansConsistent checks that spans describe the transformation from
// src to out: ordered, disjoint, inside src, source bytes between them
// unchanged in out, and sizes adding up.
func assertSpansConsistent(t *testing.T, src, out []byte, spans []EditSpan) {
	t.Helper()
	prev, pos := 0, 0
	for i, s := range spans {
		if s.Start < prev || s.End < s.Start || s.End > len(src) || s.Len < 0 {
			t.Fatalf("span %d %+v is out of order or out of range", i, s)
		}
		gap := src[prev:s.Start]
		if pos > len(out) || !bytes.HasPrefix(out[pos:], gap) {
			t.Fatalf("source bytes %d:%d do not appear unchanged at lowered offset %d", prev, s.Start, pos)
		}
		pos += len(gap) + s.Len
		prev = s.End
	}
	if pos > len(out) || !bytes.Equal(src[prev:], out[pos:]) {
		t.Fatalf("the source tail from %d does not match the lowered tail from %d", prev, pos)
	}
}

// randomText returns n tokens of source-like text: words, punctuation,
// spaces, and newlines.
func randomText(rng *rand.Rand, n int) string {
	pieces := []string{"a", "bc", "data", "os", "path", "x1", ".", "(", ")", ":=", ",", " ", " ", "\n", "\t", "é", "𝒳"}
	var b strings.Builder
	for range n {
		b.WriteString(pieces[rng.IntN(len(pieces))])
	}
	return b.String()
}

// randomLowering applies random non-overlapping edits to src. The
// replacement text mixes generated pieces and bytes copied from the
// replaced text; the segments mark the copied bytes.
func randomLowering(rng *rand.Rand, src string) ([]byte, EditMap) {
	var cuts []int
	for range 1 + rng.IntN(5) {
		cuts = append(cuts, rng.IntN(len(src)+1), rng.IntN(len(src)+1))
	}
	slices.Sort(cuts)
	var out strings.Builder
	var spans []EditSpan
	var segs segBuilder
	prev := 0
	for i := 0; i+1 < len(cuts); i += 2 {
		start, end := cuts[i], cuts[i+1]
		pre := randomText(rng, rng.IntN(5))
		post := randomText(rng, rng.IntN(5))
		out.WriteString(src[prev:start])
		segs.copy(prev, start-prev)
		if rng.IntN(3) == 0 {
			repl := randomText(rng, rng.IntN(6))
			out.WriteString(repl)
			segs.gen(len(repl))
			spans = append(spans, EditSpan{Start: start, End: end, Len: len(repl)})
		} else {
			out.WriteString(pre)
			segs.gen(len(pre))
			out.WriteString(src[start:end])
			segs.copy(start, end-start)
			out.WriteString(post)
			segs.gen(len(post))
			spans = append(spans, EditSpan{Start: start, End: end, Len: len(pre) + (end - start) + len(post)})
		}
		prev = end
	}
	out.WriteString(src[prev:])
	segs.copy(prev, len(src)-prev)
	return []byte(out.String()), EditMap{Version: EditMapVersion, Source: "r.lgo", Size: len(src), Out: out.Len(), Edits: spans, Segments: segs.done()}
}

// TestPosMapProperties is the invariant test of the position map: for
// random sources and random lowerings, every lowered byte a segment marks
// as copied maps exactly to its own original byte, and every copied run's
// lowered span maps back to its own original span.
func TestPosMapProperties(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for iter := range 400 {
		src := randomText(rng, 3+rng.IntN(60))
		out, m := randomLowering(rng, src)
		pm, err := newPosMap([]byte(src), out, m)
		if err != nil {
			t.Fatalf("iteration %d: %v\nsource %q\nmap %+v", iter, err, src, m)
		}
		assertSpansConsistent(t, []byte(src), out, m.Edits)
		checkProperties(t, iter, []byte(src), out, pm)
	}
}

// checkProperties checks the exact-origin invariants of one lowering:
// every lowered byte a segment marks as copied equals its original byte
// and maps exactly to its own original byte, and every copied run's
// lowered span maps back to its own original span. It does not require
// global monotonicity: a lowering may move or duplicate exact chunks, so
// lowered order need not follow original order and one original offset may
// have several lowered copies.
func checkProperties(t *testing.T, iter int, src, out []byte, pm *posMap) {
	t.Helper()
	for _, s := range pm.segs {
		if s.Orig < 0 {
			continue
		}
		for i := range s.Len {
			if out[s.Out+i] != src[s.Orig+i] {
				t.Fatalf("iteration %d: copied lowered %d (%q) differs from original %d (%q)", iter, s.Out+i, out[s.Out+i], s.Orig+i, src[s.Orig+i])
			}
			if back, gen := pm.origOffset(s.Out+i, leading); gen || back != s.Orig+i {
				t.Fatalf("iteration %d: lowered %d maps to (%d, %v), want (%d, false)", iter, s.Out+i, back, gen, s.Orig+i)
			}
		}
		start, _ := pm.origOffset(s.Out, leading)
		if start != s.Orig {
			t.Fatalf("iteration %d: run at %d maps to %d, want %d", iter, s.Out, start, s.Orig)
		}
		finish, _ := pm.origOffset(s.Out+s.Len, trailing)
		if finish != s.Orig+s.Len {
			t.Fatalf("iteration %d: run end at %d maps to %d, want %d", iter, s.Out+s.Len, finish, s.Orig+s.Len)
		}
		if pm.low.offset(pm.low.position(s.Out)) != s.Out {
			continue
		}
		if pm.low.offset(pm.low.position(s.Out+s.Len)) != s.Out+s.Len {
			continue
		}
		if pm.orig.offset(pm.orig.position(s.Orig)) != s.Orig {
			continue
		}
		if pm.orig.offset(pm.orig.position(s.Orig+s.Len)) != s.Orig+s.Len {
			continue
		}
		got := pm.rangeToOriginal(Range{Start: pm.low.position(s.Out), End: pm.low.position(s.Out + s.Len)})
		want := Range{Start: pm.orig.position(s.Orig), End: pm.orig.position(s.Orig + s.Len)}
		if got != want {
			t.Fatalf("iteration %d: lowered range %v maps to %v, want %v", iter, s, got, want)
		}
	}
	seen := make(map[int]struct{})
	for _, s := range pm.segs {
		if s.Orig < 0 {
			continue
		}
		for i := range s.Len {
			o := s.Orig + i
			if _, ok := seen[o]; ok {
				continue
			}
			seen[o] = struct{}{}
			low, ok := pm.lowOffset(o, leading)
			if !ok {
				t.Fatalf("iteration %d: copied original %d has no lowered position", iter, o)
			}
			if back, gen := pm.origOffset(low, leading); gen || back != o {
				t.Fatalf("iteration %d: original %d -> %d -> (%d, %v) is not the identity", iter, o, low, back, gen)
			}
		}
	}
}

// TestPosMapMovedDuplicatedExactChunks checks the legitimate shapes the
// old monotonicity property forbade: a lowering may move exact chunks out
// of original order and may duplicate one original chunk in two lowered
// places. Every lowered copy still maps exactly to its own origin, and
// every run's span maps back to that origin.
func TestPosMapMovedDuplicatedExactChunks(t *testing.T) {
	src := []byte("ABCDE FGHIJ")
	movedOut := append(append([]byte("FGHIJ "), []byte("ABCDE")...), []byte("!")...)
	moved := EditMap{
		Version: EditMapVersion, Source: "m.lgo", Size: len(src), Out: len(movedOut),
		Edits: []EditSpan{{Start: 0, End: len(src), Len: len(movedOut)}},
		Segments: []Segment{
			{Out: 0, Len: 5, Orig: 6},
			{Out: 5, Len: 1, Orig: -1},
			{Out: 6, Len: 5, Orig: 0},
			{Out: 11, Len: 1, Orig: -1},
		},
	}
	pm, err := newPosMap(src, movedOut, moved)
	if err != nil {
		t.Fatal(err)
	}
	checkProperties(t, 0, src, movedOut, pm)

	dupOut := []byte("AB AB")
	dup := EditMap{
		Version: EditMapVersion, Source: "d.lgo", Size: 2, Out: len(dupOut),
		Edits: []EditSpan{{Start: 0, End: 0, Len: 1}, {Start: 2, End: 2, Len: 2}},
		Segments: []Segment{
			{Out: 0, Len: 2, Orig: 0},
			{Out: 2, Len: 1, Orig: -1},
			{Out: 3, Len: 2, Orig: 0},
		},
	}
	dpm, err := newPosMap([]byte("AB"), dupOut, dup)
	if err != nil {
		t.Fatal(err)
	}
	checkProperties(t, 1, []byte("AB"), dupOut, dpm)
}

// TestTextOffsetsUseUTF16 checks the conversion between byte offsets and
// UTF-16 positions: multi-byte characters count one unit, characters
// beyond the basic plane count two, and a character past a line's end
// clamps to it.
func TestTextOffsetsUseUTF16(t *testing.T) {
	const doc = "a𝒳é\nxyz\r\nlast"
	tx := newText([]byte(doc))
	cases := []struct {
		off int
		pos Position
	}{
		{0, Position{0, 0}},
		{1, Position{0, 1}},
		{5, Position{0, 3}},
		{7, Position{0, 4}},
		{8, Position{1, 0}},
		{11, Position{1, 3}},
		{13, Position{2, 0}},
	}
	for _, c := range cases {
		if got := tx.position(c.off); got != c.pos {
			t.Errorf("position(%d) = %v, want %v", c.off, got, c.pos)
		}
		if got := tx.offset(c.pos); got != c.off {
			t.Errorf("offset(%v) = %d, want %d", c.pos, got, c.off)
		}
	}
	if got := tx.offset(Position{Line: 0, Character: 99}); got != 7 {
		t.Errorf("a character past the line end gives %d, want the line end 7", got)
	}
	if got := tx.offset(Position{Line: 99}); got != len(doc) {
		t.Errorf("a line past the end gives %d, want the text size", got)
	}
	if off, ok := tx.lineCol(2, 4); !ok || off != 11 {
		t.Errorf("lineCol(2, 4) = %d, %v, want 11, true", off, ok)
	}
	if _, ok := tx.lineCol(2, 40); ok {
		t.Error("lineCol accepted a column past the end of its line")
	}
}

// TestApplyChangesIncremental checks didChange handling: a range change
// splices UTF-16 positions, a whole-text change replaces the text, and
// changes apply in order.
func TestApplyChangesIncremental(t *testing.T) {
	tx := newText([]byte("héllo\nworld\n"))
	tx = applyChanges(tx, []contentChange{
		{Range: &Range{Start: Position{0, 1}, End: Position{0, 2}}, Text: "e"},
		{Range: &Range{Start: Position{1, 0}, End: Position{1, 0}}, Text: ">> "},
	})
	if got := string(tx.data); got != "hello\n>> world\n" {
		t.Errorf("after range changes: %q", got)
	}
	tx = applyChanges(tx, []contentChange{{Text: "new"}})
	if got := string(tx.data); got != "new" {
		t.Errorf("after whole-text change: %q", got)
	}
}

// mustPosMap builds the position map of src and out through m, failing
// the test when the map does not fit its texts.
func mustPosMap(t *testing.T, src, out string, m EditMap) *posMap {
	t.Helper()
	pm, err := newPosMap([]byte(src), []byte(out), m)
	if err != nil {
		t.Fatalf("newPosMap(%q, %q): %v", src, out, err)
	}
	return pm
}

// TestPosMapGeneratedPrefixAtZeroAndEmptySource checks the two insertion
// edges the map spells out in code: a generated prefix is an insertion at
// offset zero, so the trailing edge stays before it while the leading
// edge goes after it, and an empty source with generated output is one
// insertion at offset zero the same way.
func TestPosMapGeneratedPrefixAtZeroAndEmptySource(t *testing.T) {
	prefix := mustPosMap(t, "ab\n", "PRE\nab\n", EditMap{
		Version: EditMapVersion, Source: "p.lgo", Size: 3, Out: 7,
		Edits:    []EditSpan{{Start: 0, End: 0, Len: 4}},
		Segments: spanSegs(3, []EditSpan{{Start: 0, End: 0, Len: 4}}),
	})
	if got := prefix.toLowered(Position{Line: 0, Character: 0}); got != (Position{Line: 1, Character: 0}) {
		t.Errorf("source start maps to %v, want the first copied line 1:0", got)
	}
	if got := prefix.toLowered(Position{Line: 0, Character: 1}); got != (Position{Line: 1, Character: 1}) {
		t.Errorf("source 0:1 maps to %v, want lowered 1:1", got)
	}
	if got := prefix.toOriginal(Position{Line: 0, Character: 1}); got != (Position{Line: 0, Character: 0}) {
		t.Errorf("a position inside the prefix maps to %v, want the source start", got)
	}
	if got := prefix.rangeToLowered(Range{Start: Position{Line: 0, Character: 0}, End: Position{Line: 0, Character: 2}}); got != (Range{Start: Position{Line: 1, Character: 0}, End: Position{Line: 1, Character: 2}}) {
		t.Errorf("source line range maps to %v, want lowered 1:0-1:2", got)
	}
	if got := prefix.rangeToOriginal(Range{Start: Position{Line: 0, Character: 0}, End: Position{Line: 0, Character: 3}}); got != (Range{Start: Position{}, End: Position{}}) {
		t.Errorf("a range over only the prefix maps to %v, want the empty range at the source start", got)
	}
	if got, ok := prefix.lowOffset(0, trailing); !ok || got != 0 {
		t.Errorf("trailing offset zero maps to (%d, %v), want (0, true) before the prefix", got, ok)
	}

	empty := mustPosMap(t, "", "GEN", EditMap{
		Version: EditMapVersion, Source: "e.lgo", Size: 0, Out: 3,
		Edits:    []EditSpan{{Start: 0, End: 0, Len: 3}},
		Segments: spanSegs(0, []EditSpan{{Start: 0, End: 0, Len: 3}}),
	})
	if got := empty.toLowered(Position{}); got != (Position{Line: 0, Character: 3}) {
		t.Errorf("the empty source maps to %v, want the end of the generated output", got)
	}
	if got := empty.toOriginal(Position{Line: 0, Character: 1}); got != (Position{}) {
		t.Errorf("generated output of an empty source maps to %v, want the source start", got)
	}
	if got := empty.rangeToOriginal(Range{Start: Position{}, End: Position{Line: 0, Character: 3}}); got != (Range{}) {
		t.Errorf("the whole generated output maps to %v, want the empty source range", got)
	}
}

// TestPosMapMidTextInsertionSeams checks a pure insertion inside the
// text: the leading edge goes after the inserted bytes and the trailing
// edge stays before them, a range over only the inserted bytes collapses
// to its insertion point, and interior bytes anchor to that point.
func TestPosMapMidTextInsertionSeams(t *testing.T) {
	edits := []EditSpan{{Start: 1, End: 1, Len: 3}}
	pm := mustPosMap(t, "ab", "aXYZb", EditMap{
		Version: EditMapVersion, Source: "m.lgo", Size: 2, Out: 5,
		Edits: edits, Segments: spanSegs(2, edits),
	})
	if got := pm.toLowered(Position{Line: 0, Character: 1}); got != (Position{Line: 0, Character: 4}) {
		t.Errorf("the seam maps to %v, want lowered 0:4 after the insertion", got)
	}
	if got := pm.rangeToLowered(Range{Start: Position{Line: 0, Character: 0}, End: Position{Line: 0, Character: 1}}); got != (Range{Start: Position{Line: 0, Character: 0}, End: Position{Line: 0, Character: 1}}) {
		t.Errorf("the range before the insertion maps to %v, want itself", got)
	}
	if got := pm.rangeToLowered(Range{Start: Position{Line: 0, Character: 1}, End: Position{Line: 0, Character: 2}}); got != (Range{Start: Position{Line: 0, Character: 4}, End: Position{Line: 0, Character: 5}}) {
		t.Errorf("the range after the insertion maps to %v, want lowered 0:4-0:5", got)
	}
	if got := pm.toOriginal(Position{Line: 0, Character: 2}); got != (Position{Line: 0, Character: 1}) {
		t.Errorf("inserted bytes map to %v, want the insertion point 0:1", got)
	}
}

// TestPosMapMidAndEndTextDeletionNeighbors checks the neighbors of
// deleted text: a position inside it maps to the start of what replaced
// it, the leading edge at the seam sits after it, and a range across it
// widens to its original coordinates. The end-of-file deletion is the
// mirror of the covered end-of-file insertion.
func TestPosMapMidAndEndTextDeletionNeighbors(t *testing.T) {
	mid := mustPosMap(t, "one\ndropped\ntwo\n", "one\ntwo\n", EditMap{
		Version: EditMapVersion, Source: "d.lgo", Size: 16, Out: 8,
		Edits:    []EditSpan{{Start: 4, End: 12, Len: 0}},
		Segments: spanSegs(16, []EditSpan{{Start: 4, End: 12, Len: 0}}),
	})
	if got := mid.toLowered(Position{Line: 2, Character: 0}); got != (Position{Line: 1, Character: 0}) {
		t.Errorf("the line after the deletion maps to %v, want lowered 1:0", got)
	}
	if got := mid.toLowered(Position{Line: 1, Character: 0}); got != (Position{Line: 1, Character: 0}) {
		t.Errorf("a position inside deleted text maps to %v, want the start of its replacement", got)
	}
	if got := mid.toOriginal(Position{Line: 1, Character: 0}); got != (Position{Line: 2, Character: 0}) {
		t.Errorf("the seam maps to %v, want the original line after the deletion", got)
	}
	if got := mid.rangeToOriginal(Range{Start: Position{Line: 0, Character: 0}, End: Position{Line: 1, Character: 3}}); got != (Range{Start: Position{Line: 0, Character: 0}, End: Position{Line: 2, Character: 3}}) {
		t.Errorf("a range across the deletion maps to %v, want the original 0:0-2:3", got)
	}

	tail := mustPosMap(t, "keep\ngone\n", "keep\n", EditMap{
		Version: EditMapVersion, Source: "t.lgo", Size: 10, Out: 5,
		Edits:    []EditSpan{{Start: 5, End: 10, Len: 0}},
		Segments: spanSegs(10, []EditSpan{{Start: 5, End: 10, Len: 0}}),
	})
	if got := tail.toLowered(Position{Line: 1, Character: 3}); got != (Position{Line: 1, Character: 0}) {
		t.Errorf("a position inside the deleted tail maps to %v, want the lowered end", got)
	}
	if got := tail.toOriginal(Position{Line: 1, Character: 0}); got != (Position{Line: 2, Character: 0}) {
		t.Errorf("the lowered end maps to %v, want the original end past the deleted tail", got)
	}
	if got := tail.rangeToOriginal(Range{Start: Position{Line: 0, Character: 0}, End: Position{Line: 1, Character: 0}}); got != (Range{Start: Position{Line: 0, Character: 0}, End: Position{Line: 1, Character: 0}}) {
		t.Errorf("the whole lowered text maps to %v, want the original without its deleted tail", got)
	}
}

// TestNewPosMapRejectsDegenerateSegments checks the segment faults the
// table test does not name: a zero-length run, an origin below the
// generated mark, a first run that does not start the lowered text, and
// a run past the lowered text are all refused, so a corrupt sidecar
// never maps. TestNewPosMapRejectsBadMaps covers the overlapping,
// short, out-of-source, and differing-bytes faults.
func TestNewPosMapRejectsDegenerateSegments(t *testing.T) {
	src, out := []byte("abc"), []byte("aXXc")
	edits := []EditSpan{{Start: 1, End: 2, Len: 2}}
	segs := spanSegs(len(src), edits)
	cases := map[string][]Segment{
		"zero length": {
			{Out: 0, Len: 1, Orig: 0}, {Out: 1, Len: 0, Orig: -1}, {Out: 1, Len: 2, Orig: -1}, {Out: 3, Len: 1, Orig: 2},
		},
		"origin below generated": {
			{Out: 0, Len: 1, Orig: 0}, {Out: 1, Len: 2, Orig: -2}, {Out: 3, Len: 1, Orig: 2},
		},
		"gap at start": {
			{Out: 1, Len: 2, Orig: -1}, {Out: 3, Len: 1, Orig: 2},
		},
		"run past the text": {
			{Out: 0, Len: 1, Orig: 0}, {Out: 1, Len: 4, Orig: -1},
		},
	}
	for name, s := range cases {
		m := EditMap{Version: EditMapVersion, Source: "x.lgo", Size: 3, Out: 4, Edits: edits, Segments: s}
		if _, err := newPosMap(src, out, m); err == nil {
			t.Errorf("%s: degenerate map accepted", name)
		}
	}
	if _, err := newPosMap(src, out, EditMap{Version: EditMapVersion, Source: "x.lgo", Size: 3, Out: 4, Edits: edits, Segments: segs}); err != nil {
		t.Errorf("the control map is rejected: %v", err)
	}
}

// rewrittenLgo exercises the three shapes the real compiler rewrites: a
// ? statement, chain calls with expression and block lambdas, and a ?
// lambda body.
const rewrittenLgo = `package z

import (
	"fmt"
	"strconv"
)

func Parse(s string) (int, error) {
	n := strconv.Atoi(s)?
	return n, nil
}

func Double(xs []int) []int {
	return xs.map(x => x * 2)
}

func SumTwice(xs []int) []int {
	return xs.map(x => {
		return x * 2
	})
}

func First(xs []int) (string, error) {
	ys := xs.map(x => Parse(fmt.Sprint(x))?)
	return fmt.Sprint(len(ys)), nil
}
`

// TestPosMapRealLoweringKeepsRewrittenCoordinates lowers real loGo/s
// through the production compiler and checks the mapping the proxy will
// use: every user token the test names maps into lowered text starting
// with the same token and round-trips to the identity, the invariants
// hold, generated runs anchor inside the source, and every invented
// _logos_ identifier sits strictly inside generated text.
func TestPosMapRealLoweringKeepsRewrittenCoordinates(t *testing.T) {
	root, tc := realWorkspace(t)
	zdir := filepath.Join(root, "lmod", "z")
	if err := os.MkdirAll(zdir, 0o777); err != nil {
		t.Fatal(err)
	}
	zpath := filepath.Join(zdir, "z.lgo")
	if err := os.WriteFile(zpath, []byte(rewrittenLgo), 0o666); err != nil {
		t.Fatal(err)
	}
	low, err := tc.Lower(t.Context(), zdir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(low.Missing) != 0 || len(low.Files) != 1 || low.Files[0].Path != zpath {
		t.Fatalf("lowering = %+v, want the one file %s", low, zpath)
	}
	file := low.Files[0]
	out := string(file.Out)
	if strings.Contains(out, "?") {
		t.Errorf("lowered text keeps a ?:\n%s", out)
	}
	pm, err := newPosMap(file.Src, file.Out, file.Map)
	if err != nil {
		t.Fatalf("the compiler's edit map does not fit its texts: %v", err)
	}
	needles := []struct {
		text string
		nth  int
	}{
		{"strconv.Atoi(s)", 0},
		{"x * 2", 0},
		{"x * 2", 1},
		{"Parse(fmt.Sprint(x))", 0},
		{"len(ys)", 0},
	}
	for _, n := range needles {
		orig := findPos(t, rewrittenLgo, n.text, n.nth)
		lowPos := pm.toLowered(orig)
		if got := lineOf(out, lowPos.Line)[lowPos.Character:]; !strings.HasPrefix(got, n.text) {
			t.Errorf("%q maps to lowered %v, which starts %q", n.text, lowPos, got)
		}
		if back := pm.toOriginal(lowPos); back != orig {
			t.Errorf("%q round trip %v -> %v -> %v", n.text, orig, lowPos, back)
		}
	}
	checkProperties(t, 0, file.Src, file.Out, pm)
	generated := 0
	for _, s := range pm.segs {
		if s.Orig >= 0 {
			continue
		}
		generated += s.Len
		if s.Len >= 2 {
			if _, gen := pm.origOffset(s.Out+1, leading); !gen {
				t.Errorf("inside generated run %v maps as user text", s)
			}
		}
	}
	if generated == 0 {
		t.Error("the lowering carries no generated bytes")
	}
	count := 0
	for i := strings.Index(out, "_logos_"); i >= 0; {
		count++
		if _, gen := pm.origOffset(i+1, leading); !gen {
			t.Errorf("invented identifier at lowered offset %d maps as user text", i)
		}
		next := strings.Index(out[i+1:], "_logos_")
		if next < 0 {
			break
		}
		i += 1 + next
	}
	if count == 0 {
		t.Error("the lowering invents no _logos_ identifier")
	}
}
