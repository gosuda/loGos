// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package capabilities

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"cmd/go/internal/cfg"
	"cmd/go/internal/logos/config"
)

// LinkPolicy is the JSON policy cmd/go writes for the linker -logos.cap
// flag (M12-S2). Severities follow the [capabilities] table; MainModules
// holds the main module paths (first-party packages under them are never
// blamed); NestedModules holds nested module roots (modules in the build
// list under a main module path that are not themselves main modules),
// so the linker blames their packages as dependencies; CgoAllowed holds
// the import paths whose cgo use the M0 policy allowed.
//
// The wire format is encoding/json v1 with a plain struct on both sides:
// cmd/go and cmd/link build in every experiment configuration, while
// encoding/json/v2 is gated behind goexperiment.jsonv2.
type LinkPolicy struct {
	TransitiveReflect       string   `json:"transitive_reflect"`
	TransitiveExec          string   `json:"transitive_exec"`
	TransitiveNet           string   `json:"transitive_net"`
	ReflectAllowlist        []string `json:"reflect_allowlist"`
	TransitiveExecAllowlist []string `json:"transitive_exec_allowlist"`
	TransitiveNetAllowlist  []string `json:"transitive_net_allowlist"`
	MainModules             []string `json:"main_modules"`
	NestedModules           []string `json:"nested_modules"`
	CgoAllowed              []string `json:"cgo_allowed"`
}

// PolicyForLink builds the linker policy from the governing main-module
// logos.toml, every modload main module path, the recorded build modules
// (filtered to nested roots), and the recorded cgo allowances.
func PolicyForLink(cf *config.File, mainModules []string) *LinkPolicy {
	p := &LinkPolicy{CgoAllowed: CgoAllowed(), MainModules: mainModules, NestedModules: NestedModuleRoots(mainModules, BuildModules())}
	if cf == nil {
		p.TransitiveReflect = string(Warn)
		p.TransitiveExec = string(Deny)
		p.TransitiveNet = string(Deny)
		return p
	}
	p.TransitiveReflect = cf.Capabilities.TransitiveReflect
	p.TransitiveExec = cf.Capabilities.TransitiveExec
	p.TransitiveNet = cf.Capabilities.TransitiveNet
	p.ReflectAllowlist = cf.Capabilities.ReflectAllowlist
	p.TransitiveExecAllowlist = cf.Capabilities.TransitiveExecAllowlist
	p.TransitiveNetAllowlist = cf.Capabilities.TransitiveNetAllowlist
	return p
}

// MarshalPolicy encodes a linker policy as JSON.
func MarshalPolicy(p *LinkPolicy) ([]byte, error) {
	return json.Marshal(p)
}

// UnmarshalPolicy decodes a linker policy from JSON.
func UnmarshalPolicy(data []byte) (*LinkPolicy, error) {
	var p LinkPolicy
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// cgoAllowedSet records the import paths whose cgo use the M0 policy
// allowed during load. The set is process-global because one cmd/go
// invocation loads once and links after; entries are write-once.
var cgoAllowedSet = struct {
	sync.Mutex
	byPath map[string]bool
}{byPath: make(map[string]bool)}

// NoteCgoAllowed records importPath as allowed cgo use for the linker
// audit. Callers: load.checkLogosCgo records every package it allows;
// Go-module packages never reach it, so their cgo use stays unrecorded
// and the linker flags runtime.cgocall from them.
func NoteCgoAllowed(importPath string) {
	cgoAllowedSet.Lock()
	defer cgoAllowedSet.Unlock()
	cgoAllowedSet.byPath[importPath] = true
}

// CgoAllowed returns the recorded cgo-allowed import paths, sorted.
func CgoAllowed() []string {
	cgoAllowedSet.Lock()
	defer cgoAllowedSet.Unlock()
	return slices.Sorted(maps.Keys(cgoAllowedSet.byPath))
}

// NestedModuleRoots returns the sorted nested module roots among
// buildModules: module paths strictly under a main module path that are
// not themselves main modules. A nested module owns its own go.mod under
// a main-module-path directory tree but is consumed as a dependency, so
// the linker blames packages at or under its root. A path that merely
// shares a string prefix (example.com/application under
// example.com/app) is not nested; only a full segment boundary
// (main + "/") counts. A nested path that is itself a main module
// (workspace builds list every main module) stays exempt and is
// excluded. Results are deduplicated and sorted for a stable wire
// encoding and link action hash.
func NestedModuleRoots(mainModules, buildModules []string) []string {
	isMain := make(map[string]bool, len(mainModules))
	for _, m := range mainModules {
		if m != "" {
			isMain[m] = true
		}
	}
	seen := make(map[string]bool)
	var out []string
	for _, b := range buildModules {
		if b == "" || isMain[b] || seen[b] {
			continue
		}
		for _, m := range mainModules {
			if m != "" && strings.HasPrefix(b, m+"/") {
				out = append(out, b)
				seen[b] = true
				break
			}
		}
	}
	slices.Sort(out)
	return out
}

// buildModulesSet holds module paths seen during load; the mutex makes
// updates linearizable and entries are write-once per process.
var buildModulesSet = struct {
	sync.Mutex
	byPath map[string]bool
}{byPath: make(map[string]bool)}

// NoteBuildModule records modulePath as a module observed in the loaded
// package graph. Callers: load.checkLogosLoaded records every loaded
// package's module; standard-library and module-less packages never
// reach it, so only real modules are recorded.
func NoteBuildModule(modulePath string) {
	if modulePath == "" {
		return
	}
	buildModulesSet.Lock()
	defer buildModulesSet.Unlock()
	buildModulesSet.byPath[modulePath] = true
}

// BuildModules returns the recorded build module paths, sorted.
func BuildModules() []string {
	buildModulesSet.Lock()
	defer buildModulesSet.Unlock()
	return slices.Sorted(maps.Keys(buildModulesSet.byPath))
}

// linkPolicySnapshot holds the marshaled effective policy for this build,
// recorded by SnapshotLinkPolicy during link action construction (after
// load, so the cgo allowance set is final) and consumed by HashLinkPolicy
// and AppendLinkCapFlag. It is refreshed on every snapshot, so successive
// builds in one process never reuse another build's policy.
var linkPolicySnapshot = struct {
	sync.Mutex
	set  bool
	data []byte
}{}

// SnapshotLinkPolicy records the effective linker policy for this build
// from the governing main-module config and every main module path. A
// config load failure records no policy; the load phase already failed
// the build before linking.
func SnapshotLinkPolicy(cf *config.File, mainModules []string, err error) {
	linkPolicySnapshot.Lock()
	defer linkPolicySnapshot.Unlock()
	linkPolicySnapshot.set = false
	linkPolicySnapshot.data = nil
	if err != nil || cf == nil {
		return
	}
	data, merr := MarshalPolicy(PolicyForLink(cf, mainModules))
	if merr != nil {
		return
	}
	linkPolicySnapshot.set = true
	linkPolicySnapshot.data = data
}

// HashLinkPolicy mixes the snapshotted effective policy into the link
// action ID hash, so a policy change forces a relink and its audit. It
// writes nothing outside logos mode, leaving plain go action IDs
// byte-identical to upstream.
func HashLinkPolicy(h io.Writer) error {
	if !cfg.Logos {
		return nil
	}
	linkPolicySnapshot.Lock()
	data := linkPolicySnapshot.data
	set := linkPolicySnapshot.set
	linkPolicySnapshot.Unlock()
	if !set {
		return nil
	}
	sum := sha256.Sum256(data)
	_, err := fmt.Fprintf(h, "logoslinkcap %x\n", sum)
	return err
}

// AppendLinkCapFlag appends the linker flag carrying the capabilities
// policy for one linked binary, or returns ldflags unchanged outside
// logos mode. The policy is written to the link action's own object
// directory, never to a shared temporary file, so a pre-existing file
// cannot substitute another policy. A config load failure appends
// nothing; the load phase already failed the build before linking.
func AppendLinkCapFlag(ldflags []string, objdir string) []string {
	if !cfg.Logos {
		return ldflags
	}
	linkPolicySnapshot.Lock()
	data := linkPolicySnapshot.data
	set := linkPolicySnapshot.set
	linkPolicySnapshot.Unlock()
	if !set {
		return ldflags
	}
	path := filepath.Join(objdir, "logos-cap.json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		return ldflags
	}
	return append(ldflags, "-logos.cap="+path)
}
