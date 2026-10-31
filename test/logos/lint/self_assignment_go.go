// errorcheck

// suspicious::self_assignment on a Go package (LINT-RULES.md Sec 1.2): detects
// useless self-assignments (x = x, s.f = s.f).

package p

type S struct {
	f int
}

func badAssignments(x int, s S) {
	x = x // ERROR "\[suspicious::self_assignment\]" "\[nogo::assign\]"
	s.f = s.f // ERROR "\[suspicious::self_assignment\]" "\[nogo::assign\]"
	var a, b int
	a, b = a, b // ERROR "\[suspicious::self_assignment\]" "\[nogo::assign\]"
	a, b = a, 1 // ERROR "\[suspicious::self_assignment\]" "\[nogo::assign\]"
	_ = a
	_ = b
}

func cleanAssignments(x, y int, s, other S) {
	x = y
	s.f = other.f
}

func cleanMap(m map[string]int) {
	m["k"] = m["k"]
}

//logos:allow(suspicious::self_assignment)
//logos:allow(nogo::assign)
func allowedInGo(x int) {
	x = x
}
