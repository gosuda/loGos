// run -O=3

package main

import (
	"fmt"
	"io"
	"net/http"
)

// M13-S3 row #124 (SPEC T-203.4), Go twin of specialize_assert.lgo: a
// dynamic w.(http.Flusher) inside a function that specializes gives the
// same answer per concrete type as the original gives through an interface
// value. emit specializes for *plainWriter (no Flush, so the assertion
// folds to false) and for *flushWriter (it folds to true); a call that
// passes an io.Writer value keeps the original, whose assertion stays
// dynamic.

type plainWriter struct{ n int }

func (p *plainWriter) Write(b []byte) (int, error) {
	p.n += len(b)
	return len(b), nil
}

type flushWriter struct{ n, flushes int }

func (f *flushWriter) Write(b []byte) (int, error) {
	f.n += len(b)
	return len(b), nil
}

func (f *flushWriter) Flush() { f.flushes++ }

func emit(w io.Writer, msg string) string {
	if _, err := w.Write([]byte(msg)); err != nil {
		return err.Error()
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
		return "flushed"
	}
	return "buffered"
}

func main() {
	var pw plainWriter
	var fw flushWriter
	fmt.Println(emit(&pw, "a"), emit(&fw, "bc"), emit(&fw, "d"))
	fmt.Println(pw.n, fw.n, fw.flushes)

	var dyn io.Writer = &fw
	got := emit(dyn, "xyz")
	fmt.Println(got, fw.flushes)
}
