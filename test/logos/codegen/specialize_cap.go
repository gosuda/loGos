// asmcheck -O=2 -d=logosspecialize=1

package main

// M13-S3 cost cap (SPEC T-203.4), Go twin of specialize_cap.lgo: drain's body has 32 statements of seven
// syntax nodes each, above the 200-node cap of the capped tiers, so at -O=2
// with -d=logosspecialize=1 the call keeps the original drain. recover keeps
// drain out of the inliner. small is the control: it fits under the cap, so
// its call specializes at the same tier and the missing drain clone can only
// come from the cap. specialize_cap_o3.go shows the same call
// specializing at -O=3, where no cost limit exists.

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

func small(w sink, b []byte) int {
	_ = recover()
	return w.Put(b)
}

func main() {
	r := &ring{}
	println(small(r, []byte("ab"))) // amd64:`CALL.*small.logosspec[0-9]+` arm64:`CALL.*small.logosspec[0-9]+`
	println(drain(r, []byte("ab"))) // amd64:`CALL.*main\.drain\(SB\)` amd64:-`CALL.*drain.logosspec` arm64:`CALL.*main\.drain\(SB\)` arm64:-`CALL.*drain.logosspec`
}
