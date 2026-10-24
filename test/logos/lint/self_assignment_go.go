// errorcheck -0

// suspicious::self_assignment on a Go package (LINT-RULES.md Sec 1.2): detects
// useless self-assignments (x = x, s.f = s.f).

package p

type S struct {
	f int
}

func badAssignments(x int, s S) {
	x = x // ERROR "\[suspicious::self_assignment\]"
	s.f = s.f // ERROR "\[suspicious::self_assignment\]"
	var a, b int
	a, b = a, b // ERROR "\[suspicious::self_assignment\]"
	a, b = a, 1 // ERROR "\[suspicious::self_assignment\]"
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
func allowedInGo(x int) {
	x = x
}
