// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package json

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

var sinkInt int

var sinkBytes []byte

type benchUser struct {
	ID    int64
	Name  string
	Email string
	Age   int
	Bio   string
	Score int64
}

var benchUsers = []benchUser{
	{ID: 1, Name: "Ada Lovelace", Email: "ada@example.com", Age: 36, Bio: "Analyst and mathematician", Score: 99},
	{ID: 123456789, Name: "Grace <Hopper> & Co", Email: "grace@example.org", Age: 85, Bio: "Rear admiral \"Amazing Grace\"\nCOBOL", Score: -5},
	{ID: math.MaxInt64, Name: "Unicode \u2028 Name \xff", Email: "x@y.z", Age: 0, Bio: strings.Repeat("long biography ", 20), Score: math.MinInt64},
}

func BenchmarkJsonMarshalBound(b *testing.B) {
	b.ReportAllocs()
	var n int
	for i := range b.N {
		u := &benchUsers[i%len(benchUsers)]
		n += 2 + IntLen(u.ID) + EscapedLen(u.Name) + EscapedLen(u.Email) + IntLen(int64(u.Age)) + EscapedLen(u.Bio) + IntLen(u.Score)
	}
	sinkInt = n
}

func BenchmarkJsonIntLen(b *testing.B) {
	vals := []int64{0, 7, 42, 1234, 99999, 123456789, -5, -1234567, math.MaxInt64, math.MinInt64}
	b.Run("IntLen", func(b *testing.B) {
		var n int
		for i := range b.N {
			n += IntLen(vals[i%len(vals)])
		}
		sinkInt = n
	})
	b.Run("AppendIntThenLen", func(b *testing.B) {
		var n int
		var buf [24]byte
		for i := range b.N {
			n += len(strconv.AppendInt(buf[:0], vals[i%len(vals)], 10))
		}
		sinkInt = n
	})
}

func BenchmarkJsonEscapeScan(b *testing.B) {
	corpora := []struct {
		name string
		s    string
	}{
		{"NoEscape", strings.Repeat("The quick brown fox jumps over the lazy dog 0123456789. ", 18)},
		{"NoEscapeUnicode", strings.Repeat("日本語のテキスト and plain ascii mixed in. ", 24)},
		{"Mixed", strings.Repeat("quote \" tab \t <b>bold</b> & amp \u2028 \xff end. ", 20)},
	}
	for _, c := range corpora {
		b.Run(c.name+"/EscapedLen", func(b *testing.B) {
			b.SetBytes(int64(len(c.s)))
			var n int
			for range b.N {
				n += EscapedLen(c.s)
			}
			sinkInt = n
		})
		b.Run(c.name+"/AppendEscaped", func(b *testing.B) {
			b.SetBytes(int64(len(c.s)))
			buf := make([]byte, 0, 2*len(c.s)+64)
			for range b.N {
				buf = AppendEscaped(buf[:0], c.s)
			}
			sinkBytes = buf
		})
	}
}

func BenchmarkJsonSliceInt(b *testing.B) {
	data := []byte(`{"A":[` + strings.Repeat("12345,", 999) + `1]}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	for range b.N {
		got, err := decodeSingleBytes(data, sliceOf(Int[int]))
		if err != nil || len(got.A) != 1000 {
			b.Fatal(err)
		}
	}
}
