// asmcheck -O=3

package main

// M13-S3 block rules (SPEC T-203.4), Go twin of specialize_block.lgo: a
// parameter compared with nil, reassigned, or stored in a struct field
// blocks specialization for that call, so the original function runs.
// clean has the same shape without any of those uses and is the control: its
// call specializes at the same tier. Each callee calls recover, which keeps
// it out of the inliner, and calls a method on the parameter, so only the
// block rule explains a missing clone.

type sink interface {
	Put(p []byte) int
}

type ring struct{ n int }

//go:noinline
func (r *ring) Put(p []byte) int {
	r.n += len(p)
	return r.n
}

type holder struct{ w sink }

func clean(w sink) int {
	_ = recover()
	return w.Put(nil)
}

func nilCmp(w sink) int {
	_ = recover()
	if w == nil {
		return 0
	}
	return w.Put(nil)
}

func reassign(w sink) int {
	_ = recover()
	w = &ring{}
	return w.Put(nil)
}

func store(h *holder, w sink) int {
	_ = recover()
	h.w = w
	return w.Put(nil)
}

func main() {
	r := &ring{}
	var h holder
	println(clean(r))     // amd64:`CALL.*clean.logosspec[0-9]+` arm64:`CALL.*clean.logosspec[0-9]+`
	println(nilCmp(r))    // amd64:`CALL.*main\.nilCmp\(SB\)` amd64:-`CALL.*nilCmp.logosspec` arm64:`CALL.*main\.nilCmp\(SB\)` arm64:-`CALL.*nilCmp.logosspec`
	println(reassign(r))  // amd64:`CALL.*main\.reassign\(SB\)` amd64:-`CALL.*reassign.logosspec` arm64:`CALL.*main\.reassign\(SB\)` arm64:-`CALL.*reassign.logosspec`
	println(store(&h, r)) // amd64:`CALL.*main\.store\(SB\)` amd64:-`CALL.*store.logosspec` arm64:`CALL.*main\.store\(SB\)` arm64:-`CALL.*store.logosspec`
}
