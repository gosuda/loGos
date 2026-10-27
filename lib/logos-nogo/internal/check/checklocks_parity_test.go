// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package check

import (
	"strings"
	"testing"
)

// Reference: tools/checklocks/test/basics.go. checklocks recognises locks by
// the Mutex/RWMutex name pattern (internal/checklocks/facts.go: lockKind),
// the same shape as gvisor's own sync.Mutex, so the fixture declares a
// minimal local Mutex exercising that matching contract on the production
// checkPackage path (real parsing, type checking, SSA construction via
// buildssa, guard resolution). Fixture bodies are analysis-only and never
// execute.
const parityLocksFixture = `package consumer

// Mutex is a minimal lock type recognised by checklocks.
type Mutex struct {
	locked bool
}

// Lock acquires m.
func (m *Mutex) Lock() {
	m.locked = true
}

// Unlock releases m.
func (m *Mutex) Unlock() {
	m.locked = false
}

// Guarded carries a field protected by mu.
type Guarded struct {
	mu Mutex
	// +checklocks:mu
	value int
}
`

func TestChecklocksParity(t *testing.T) {
	t.Run("unlocked_access", func(t *testing.T) {
		const accessors = `
// Unlocked reads value without holding mu and must be diagnosed.
func Unlocked(g *Guarded) int {
	return g.value
}

// Locked reads value while holding mu and must stay silent.
func Locked(g *Guarded) int {
	g.mu.Lock()
	v := g.value
	g.mu.Unlock()
	return v
}
`
		i := nogoParityImporter(t, "checklocks")
		nogoParityWrite(t, i, "consumer", parityLocksFixture+accessors)
		_, findings, _, err := i.checkPackage("consumer", i.sources["consumer"])
		if err != nil {
			t.Fatal(err)
		}
		wantLine := nogoParityLine(t, parityLocksFixture+accessors, "return g.value")
		if len(findings) != 1 {
			t.Fatalf("findings = %v, want exactly one diagnostic for the unlocked read", findings)
		}
		got := findings[0]
		if got.Category != "checklocks" || got.Position.Line != wantLine || !strings.Contains(got.Message, "must be locked when accessing") {
			t.Fatalf("finding = %+v, want category checklocks line %d naming the unlocked access", got, wantLine)
		}
	})
	t.Run("locked_access", func(t *testing.T) {
		i := nogoParityImporter(t, "checklocks")
		nogoParityWrite(t, i, "consumer", parityLocksFixture+`
// Locked reads value while holding mu and must stay silent.
func Locked(g *Guarded) int {
	g.mu.Lock()
	v := g.value
	g.mu.Unlock()
	return v
}
`)
		_, findings, _, err := i.checkPackage("consumer", i.sources["consumer"])
		if err != nil {
			t.Fatal(err)
		}
		if len(findings) != 0 {
			t.Fatalf("findings = %v, want no diagnostics for a guarded access", findings)
		}
	})
}
