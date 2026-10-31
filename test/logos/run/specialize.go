// run -O=3

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
)

// M13-S3 clone specialization (SPEC T-203.4), Go twin of specialize.lgo.
// At -O=3 a same-package call with concrete arguments goes to a clone whose
// interface parameters are retyped to the concrete types. Every behavior
// below must read the same at -O=1 (specialize_o1.go runs this body there):
// the original function stays for function values and for the calls the
// rules block, a parameter compared with nil, reassigned, or stored never
// clones, types nested deeper than four and callees seen with more than
// four concrete types never specialize, no destination-passing exists, and
// traces print the original name without the ·logosspec suffix.

type counter struct{ writes, bytes int }

func (ct *counter) Write(p []byte) (int, error) {
	ct.writes++
	ct.bytes += len(p)
	return len(p), nil
}

// Copy uses both parameters only as method receivers, so each concrete
// call gets a clone.
func Copy(w io.Writer, r io.Reader) (int64, error) {
	buf := make([]byte, 4)
	var n int64
	for {
		k, err := r.Read(buf)
		if k > 0 {
			m, werr := w.Write(buf[:k])
			n += int64(m)
			if werr != nil {
				return n, werr
			}
		}
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, err
		}
	}
}

// sliceWriter is a nil-able type that works with a nil value, so an
// interface holding a nil sliceWriter is a non-nil interface.
type sliceWriter []byte

func (s sliceWriter) Write(p []byte) (int, error) { return len(p), nil }

// isNil compares its parameter with nil and calls a method on it, so the
// nil rule alone blocks the clone: a clone retyped to sliceWriter would
// compare the slice and answer "nil interface" for a nil sliceWriter.
func isNil(w io.Writer) string {
	if w == nil {
		return "nil interface"
	}
	if _, err := w.Write(nil); err != nil {
		return err.Error()
	}
	return fmt.Sprintf("non-nil %T", w)
}

// replaced reassigns its parameter after a method call.
func replaced(w io.Writer) string {
	if _, err := w.Write(nil); err != nil {
		return err.Error()
	}
	w = io.Discard
	return fmt.Sprintf("%T", w)
}

type holder struct{ w io.Writer }

// keep stores its parameter in a struct field after a method call.
func keep(h *holder, w io.Writer) error {
	if _, err := w.Write(nil); err != nil {
		return err
	}
	h.w = w
	return nil
}

type Namer interface{ Name() string }

type leaf struct{}

func (leaf) Name() string { return "leaf" }

type wrap[T Namer] struct{ in T }

func (w wrap[T]) Name() string { return "(" + w.in.Name() + ")" }

func show(n Namer) string { return n.Name() }

type ider interface{ ID() int }

type a struct{}
type b struct{}
type c struct{}
type d struct{}
type e struct{}

func (a) ID() int { return 1 }
func (b) ID() int { return 2 }
func (c) ID() int { return 3 }
func (d) ID() int { return 4 }
func (e) ID() int { return 5 }

// id is called with five concrete types, so it is megamorphic; id4 with
// four.
func id(x ider) int  { return x.ID() }
func id4(x ider) int { return x.ID() }

func Reverse(xs []int) []int {
	out := make([]int, len(xs))
	for i, x := range xs {
		out[len(xs)-1-i] = x
	}
	return out
}

// where and trace call recover, which keeps them out of the inliner, so
// their frames are real and carry the symbol name of the function that runs.
func where(w io.Writer) string {
	_ = recover()
	if _, err := w.Write(nil); err != nil {
		return err.Error()
	}
	pc, _, _, _ := runtime.Caller(0)
	return runtime.FuncForPC(pc).Name()
}

func trace(w io.Writer) string {
	_ = recover()
	if _, err := w.Write(nil); err != nil {
		return err.Error()
	}
	pcs := make([]uintptr, 4)
	n := runtime.Callers(1, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	f, _ := frames.Next()
	return f.Function
}

func main() {
	var dst bytes.Buffer
	n, err := Copy(&dst, strings.NewReader("hello, clone"))
	fmt.Println(n, err, dst.String())

	var cnt counter
	n, err = Copy(&cnt, bytes.NewReader([]byte("0123456789")))
	fmt.Println(n, err, cnt.writes, cnt.bytes)

	var fn func(io.Writer, io.Reader) (int64, error) = Copy
	var viaFn bytes.Buffer
	n, err = fn(&viaFn, strings.NewReader("by value"))
	fmt.Println(n, err, viaFn.String())

	fmt.Println(isNil(sliceWriter(nil)))
	fmt.Println(isNil(nil))
	fmt.Println(replaced(&dst))
	var h holder
	if err := keep(&h, &dst); err != nil {
		fmt.Println(err)
	}
	fmt.Printf("%T\n", h.w)

	fmt.Println(show(leaf{}), show(wrap[leaf]{}), show(wrap[wrap[wrap[leaf]]]{}), show(wrap[wrap[wrap[wrap[leaf]]]]{}))

	fmt.Println(id(a{})+id(b{})+id(c{})+id(d{})+id(e{}), id4(a{})+id4(b{})+id4(c{})+id4(d{}))

	arr := []int{1, 2, 3, 4, 5}
	arr = Reverse(arr)
	fmt.Println(arr)
	arr = Reverse(arr[1:4])
	fmt.Println(arr)

	total := 0
	for i := range 6 {
		if i%2 == 0 {
			total += id4(a{})
		} else {
			total += id4(b{})
		}
	}
	fmt.Println("alternating", total)

	fmt.Println(where(&dst))
	fmt.Println(trace(&dst))
}
