// asmcheck -O=3

package main

// M13-S3 cost cap (SPEC T-203.4), Go twin of specialize_cap.lgo: the callee of specialize_cap.go, with
// 32 statements of seven syntax nodes each (over the 200-node cap of the
// capped tiers), specializes at -O=3, where no cost limit exists.

type sink interface {
	Put(p []byte) int
}

type ring struct{ n int }

//go:noinline
func (r *ring) Put(p []byte) int {
	r.n += len(p)
	return r.n
}

func drain(w sink, b []byte) int {
	_ = recover()
	total := 0
	total += w.Put(b)
	total += w.Put(b)
	total += w.Put(b)
	total += w.Put(b)
	total += w.Put(b)
	total += w.Put(b)
	total += w.Put(b)
	total += w.Put(b)
	total += w.Put(b)
	total += w.Put(b)
	total += w.Put(b)
	total += w.Put(b)
	total += w.Put(b)
	total += w.Put(b)
	total += w.Put(b)
	total += w.Put(b)
	total += w.Put(b)
	total += w.Put(b)
	total += w.Put(b)
	total += w.Put(b)
	total += w.Put(b)
	total += w.Put(b)
	total += w.Put(b)
	total += w.Put(b)
	total += w.Put(b)
	total += w.Put(b)
	total += w.Put(b)
	total += w.Put(b)
	total += w.Put(b)
	total += w.Put(b)
	total += w.Put(b)
	total += w.Put(b)
	return total
}

func main() {
	r := &ring{}
	println(drain(r, []byte("ab"))) // amd64:`CALL.*drain.logosspec[0-9]+` arm64:`CALL.*drain.logosspec[0-9]+`
}
