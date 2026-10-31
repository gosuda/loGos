// run

package main

import (
	"fmt"

	"logos/rt"
)

// M13-S3 row #121 (SPEC T-203.4), typed-nil companion to
// const_slice_covariance.lgo. Plain Go has no non-nil pointer rule, so the
// typed-nil half of the covariance contract lives here legally: a nil
// *Monster converts through rt.CovSlice to a non-nil Creature holding a nil
// receiver, and the pointer method handles that nil receiver. A nil source
// slice stays nil while an empty source stays a non-nil empty slice, and a
// nil interface element stays a nil Creature.

type Creature interface {
	pointerName() string
}

type Monster struct{ name string }

func (m *Monster) pointerName() string {
	if m == nil {
		return "<nil>"
	}
	return m.name
}

func main() {
	typed := rt.CovSlice[Creature]([]*Monster{nil, {name: "imp"}})
	fmt.Println(typed[0] == nil, typed[0].pointerName(), typed[1].pointerName())
	fmt.Println(len(typed))

	var none []*Monster
	fmt.Println(rt.CovSlice[Creature](none) == nil)
	empty := rt.CovSlice[Creature]([]*Monster{})
	fmt.Println(empty == nil, len(empty))

	ifaces := rt.CovSlice[Creature]([]Creature{nil, &Monster{name: "orc"}})
	fmt.Println(ifaces[0] == nil, ifaces[1].pointerName())
}
