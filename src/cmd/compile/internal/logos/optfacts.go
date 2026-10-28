// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package logos

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"cmd/compile/internal/logos/facts"
	"cmd/compile/internal/types2"
)

// This file carries the M3-S7 option facts (SPEC T-Sec6, T-202). The
// writer emits lowered Go-facing types in __.PKGDEF; EmitOptionFacts
// records one opt line per option position for .lgo importers, and
// CloneWithOverlay rebuilds those positions as Option for a .lgo view
// using only the public types2 API. Go importers keep the lowered types.

func optAsOption(t types2.Type) *types2.Option {
	if t == nil {
		return nil
	}
	if o, ok := types2.Unalias(t).(*types2.Option); ok {
		return o
	}
	if n, ok := types2.Unalias(t).(*types2.Named); ok {
		if u := n.Underlying(); u != nil {
			if o, ok := u.(*types2.Option); ok {
				return o
			}
		}
	}
	return nil
}

// EmitOptionFacts returns one opt record per option position in the
// exported declarations of pkg, or nil when there are none. Objects are
// func names, type names, and T.M or (*T).M methods; paths are R<i>,
// P<i>, F<name>, E, K, D, M<name> steps. A nested option extends its
// parent with E, so (T?)? yields R0 and R0.E.
func EmitOptionFacts(pkg *types2.Package, _ *types2.Info) *facts.Facts {
	if pkg == nil || pkg.Scope() == nil {
		return nil
	}
	var opts []facts.PathFact
	emit := func(object, path string) {
		opts = append(opts, facts.PathFact{Object: object, Path: path})
	}
	scope := pkg.Scope()
	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		if obj == nil || !obj.Exported() {
			continue
		}
		switch obj := obj.(type) {
		case *types2.Func:
			sig, ok := obj.Type().(*types2.Signature)
			if !ok || sig.Recv() != nil {
				continue
			}
			if tp := sig.TypeParams(); tp != nil && tp.Len() > 0 {
				continue
			}
			emitFuncOpts(name, sig, emit)
		case *types2.TypeName:
			if !obj.IsAlias() {
				emitTypeOpts(name, obj.Type(), emit)
			}
		}
	}
	for _, name := range scope.Names() {
		obj, ok := scope.Lookup(name).(*types2.TypeName)
		if !ok || obj == nil || !obj.Exported() || obj.IsAlias() {
			continue
		}
		named, ok := obj.Type().(*types2.Named)
		if !ok {
			continue
		}
		for i := range named.NumMethods() {
			m := named.Method(i)
			if m == nil || !m.Exported() {
				continue
			}
			sig, ok := m.Type().(*types2.Signature)
			if !ok {
				continue
			}
			if tp := sig.TypeParams(); tp != nil && tp.Len() > 0 {
				continue
			}
			emitFuncOpts(methodObject(name, sig.Recv(), m.Name()), sig, emit)
		}
	}
	if len(opts) == 0 {
		return nil
	}
	return &facts.Facts{Opts: opts}
}

func methodObject(typeName string, recv *types2.Var, method string) string {
	if recv == nil {
		return typeName + "." + method
	}
	if _, ok := types2.Unalias(recv.Type()).(*types2.Pointer); ok {
		return "(*" + typeName + ")." + method
	}
	return typeName + "." + method
}

func emitFuncOpts(object string, sig *types2.Signature, emit func(string, string)) {
	if params := sig.Params(); params != nil {
		for i := range params.Len() {
			v := params.At(i)
			if v == nil {
				continue
			}
			walkOptType(v.Type(), []string{fmt.Sprintf("P%d", i)}, object, emit, make(map[types2.Type]bool))
		}
	}
	if results := sig.Results(); results != nil {
		for i := range results.Len() {
			v := results.At(i)
			if v == nil {
				continue
			}
			walkOptType(v.Type(), []string{fmt.Sprintf("R%d", i)}, object, emit, make(map[types2.Type]bool))
		}
	}
}

func emitTypeOpts(name string, t types2.Type, emit func(string, string)) {
	u, ok := types2.Unalias(t).(*types2.Named)
	if !ok {
		return
	}
	if tp := u.TypeParams(); tp != nil && tp.Len() > 0 {
		return
	}
	switch u := u.Underlying().(type) {
	case *types2.Struct:
		for i := range u.NumFields() {
			f := u.Field(i)
			if f == nil || !f.Exported() {
				continue
			}
			walkOptType(f.Type(), []string{"F" + f.Name()}, name, emit, make(map[types2.Type]bool))
		}
	case *types2.Interface:
		for i := range u.NumExplicitMethods() {
			m := u.ExplicitMethod(i)
			if m == nil || !m.Exported() {
				continue
			}
			sig, ok := m.Type().(*types2.Signature)
			if !ok {
				continue
			}
			base := []string{"M" + m.Name()}
			if params := sig.Params(); params != nil {
				for j := range params.Len() {
					v := params.At(j)
					if v == nil {
						continue
					}
					walkOptType(v.Type(), append(slices.Clone(base), fmt.Sprintf("P%d", j)), name, emit, make(map[types2.Type]bool))
				}
			}
			if results := sig.Results(); results != nil {
				for j := range results.Len() {
					v := results.At(j)
					if v == nil {
						continue
					}
					walkOptType(v.Type(), append(slices.Clone(base), fmt.Sprintf("R%d", j)), name, emit, make(map[types2.Type]bool))
				}
			}
		}
	}
}

func walkOptType(t types2.Type, path []string, object string, emit func(string, string), seen map[types2.Type]bool) {
	walkQualType(t, path, object, emit, nil, seen)
}

// walkQualType is the one traversal owner for qualifier facts: it visits
// every qualifier position once, invoking emitOpt at option positions
// and emitConst at const-view positions. A nil emitter still descends
// through that qualifier, so an option nested in a const view (and the
// reverse) records its own fact instead of vanishing at the foreign
// layer. Cycle protection stays on named types only, with path-scoped
// release so diamonds revisit.
func walkQualType(t types2.Type, path []string, object string, emitOpt, emitConst func(string, string), seen map[types2.Type]bool) {
	if t == nil {
		return
	}
	if opt := optAsOption(t); opt != nil {
		if emitOpt != nil {
			emitOpt(object, joinPath(path))
		}
		walkQualType(opt.Elem, append(slices.Clone(path), "E"), object, emitOpt, emitConst, seen)
		return
	}
	if c := constAsView(t); c != nil {
		if emitConst != nil {
			emitConst(object, joinPath(path))
		}
		walkQualType(c.Elem, append(slices.Clone(path), "E"), object, emitOpt, emitConst, seen)
		return
	}
	u := types2.Unalias(t)
	switch u := u.(type) {
	case *types2.Slice:
		walkQualType(u.Elem(), append(slices.Clone(path), "E"), object, emitOpt, emitConst, seen)
	case *types2.Array:
		walkQualType(u.Elem(), append(slices.Clone(path), "E"), object, emitOpt, emitConst, seen)
	case *types2.Map:
		walkQualType(u.Key(), append(slices.Clone(path), "K"), object, emitOpt, emitConst, seen)
		walkQualType(u.Elem(), append(slices.Clone(path), "E"), object, emitOpt, emitConst, seen)
	case *types2.Pointer:
		walkQualType(u.Elem(), append(slices.Clone(path), "D"), object, emitOpt, emitConst, seen)
	case *types2.Chan:
		walkQualType(u.Elem(), append(slices.Clone(path), "D"), object, emitOpt, emitConst, seen)
	case *types2.Signature:
		if params := u.Params(); params != nil {
			for i := range params.Len() {
				v := params.At(i)
				if v == nil {
					continue
				}
				walkQualType(v.Type(), append(slices.Clone(path), fmt.Sprintf("P%d", i)), object, emitOpt, emitConst, seen)
			}
		}
		if results := u.Results(); results != nil {
			for i := range results.Len() {
				v := results.At(i)
				if v == nil {
					continue
				}
				walkQualType(v.Type(), append(slices.Clone(path), fmt.Sprintf("R%d", i)), object, emitOpt, emitConst, seen)
			}
		}
	case *types2.Struct:
		for i := range u.NumFields() {
			f := u.Field(i)
			if f == nil || !f.Exported() {
				continue
			}
			walkQualType(f.Type(), append(slices.Clone(path), "F"+f.Name()), object, emitOpt, emitConst, seen)
		}
	case *types2.Interface:
		for i := range u.NumExplicitMethods() {
			m := u.ExplicitMethod(i)
			if m == nil || !m.Exported() {
				continue
			}
			sig, ok := m.Type().(*types2.Signature)
			if !ok {
				continue
			}
			base := append(slices.Clone(path), "M"+m.Name())
			if params := sig.Params(); params != nil {
				for j := range params.Len() {
					v := params.At(j)
					if v == nil {
						continue
					}
					walkQualType(v.Type(), append(slices.Clone(base), fmt.Sprintf("P%d", j)), object, emitOpt, emitConst, seen)
				}
			}
			if results := sig.Results(); results != nil {
				for j := range results.Len() {
					v := results.At(j)
					if v == nil {
						continue
					}
					walkQualType(v.Type(), append(slices.Clone(base), fmt.Sprintf("R%d", j)), object, emitOpt, emitConst, seen)
				}
			}
		}
	case *types2.Named:
		if seen[u] {
			return
		}
		seen[u] = true
		walkQualType(u.Underlying(), path, object, emitOpt, emitConst, seen)
		delete(seen, u)
	}
}

func joinPath(path []string) string {
	return strings.Join(path, ".")
}

// qualKind identifies which qualifier an overlay path restores.
type qualKind uint8

const (
	qualOption qualKind = iota
	qualConst
)

// qualPath is one overlay path with the qualifier it restores. Opt
// entries precede const entries in application order; the emitters never
// record both qualifiers at one leaf, so a shared leaf would restore
// Option inside ConstView, matching a `const (T?)` nesting.
type qualPath struct {
	path string
	qual qualKind
}

// CloneWithOverlay restores option facts using the package graph overlay.
// The original package remains the lowered Go-facing view.
func CloneWithOverlay(orig *types2.Package, opts []facts.PathFact) *types2.Package {
	return CloneWithOverlays(orig, opts, nil)
}

// CloneWithOverlays rebuilds the exported declarations named by opts as
// Option and by consts as ConstView for a .lgo view and returns the new
// package. Both qualifiers compose in one package clone so neither pass
// loses the other's wrappers or nominal identities. The input package is
// untouched, so Go importers keep the lowered types. Only func
// results/params, struct fields, and method results/params at the
// recorded paths are qualified; all references to affected nominals,
// including unfacted declarations and aliases, use the same clone.
// Genuinely unaffected nodes share the input. A lowered bare payload
// wraps as Option or ConstView; an
// instantiated logos/rt.Option[T] becomes Option with Elem T. Restored
// const views are plain (ConstMap false): the facts carry no const-map
// mark, so none is invented.
func CloneWithOverlays(orig *types2.Package, opts, consts []facts.PathFact) *types2.Package {
	if orig == nil || (len(opts) == 0 && len(consts) == 0) {
		return orig
	}
	byObj := make(map[string][]qualPath)
	add := func(f facts.PathFact, q qualKind) {
		if f.Object != "" && f.Path != "" {
			byObj[f.Object] = append(byObj[f.Object], qualPath{path: f.Path, qual: q})
		}
	}
	for _, o := range opts {
		add(o, qualOption)
	}
	for _, c := range consts {
		add(c, qualConst)
	}
	if len(byObj) == 0 {
		return orig
	}
	for obj := range byObj {
		paths := byObj[obj]
		slices.SortStableFunc(paths, func(a, b qualPath) int {
			if d := strings.Count(a.path, ".") - strings.Count(b.path, "."); d != 0 {
				return d
			}
			if d := strings.Compare(a.path, b.path); d != 0 {
				return d
			}
			return int(a.qual) - int(b.qual)
		})
	}
	out := types2.NewPackage(orig.Path(), orig.Name())
	out.SetImports(orig.Imports())
	graph := newOverlayGraph(orig, out, byObj)
	graph.prepare()
	for _, name := range orig.Scope().Names() {
		obj := orig.Scope().Lookup(name)
		switch obj := obj.(type) {
		case *types2.TypeName:
			t := substType(graph.sub, obj.Type())
			if t != obj.Type() {
				switch t := t.(type) {
				case *types2.Named:
					out.Scope().Insert(t.Obj())
				case *types2.Alias:
					out.Scope().Insert(t.Obj())
				default:
					out.Scope().Insert(types2.NewTypeName(obj.Pos(), out, obj.Name(), t))
				}
			} else {
				out.Scope().Insert(obj)
			}
		case *types2.Func:
			sig := substSignature(graph.sub, obj.Type().(*types2.Signature))
			for _, p := range byObj[name] {
				sig = wrapAtQual(out, sig, strings.Split(p.path, "."), p.qual).(*types2.Signature)
			}
			if sig == obj.Type() {
				out.Scope().Insert(obj)
			} else {
				out.Scope().Insert(types2.NewFunc(obj.Pos(), out, obj.Name(), sig))
			}
		case *types2.Const:
			t := substType(graph.sub, obj.Type())
			if t == obj.Type() {
				out.Scope().Insert(obj)
			} else {
				out.Scope().Insert(types2.NewConst(obj.Pos(), out, obj.Name(), t, obj.Val()))
			}
		case *types2.Var:
			t := substType(graph.sub, obj.Type())
			if t == obj.Type() {
				out.Scope().Insert(obj)
			} else {
				v := types2.NewVar(obj.Pos(), out, obj.Name(), t)
				v.SetKind(obj.Kind())
				out.Scope().Insert(v)
			}
		default:
			out.Scope().Insert(obj)
		}
	}
	out.MarkComplete()
	return out
}

// overlayGraph owns the nominal closure for one imported package view.
// Shells are allocated before any bounds, underlying types, or methods are
// rebuilt. Thus recursive and mutually recursive references always resolve
// to the same nominal, including references from declarations without facts.
type overlayGraph struct {
	orig     *types2.Package
	sub      *typeSubst
	paths    map[string][]qualPath
	nodes    map[types2.Type]bool
	deps     map[types2.Type]map[types2.Type]bool
	affected map[types2.Type]bool
}

func newOverlayGraph(orig, pkg *types2.Package, paths map[string][]qualPath) *overlayGraph {
	return &overlayGraph{
		orig: orig, paths: paths,
		sub:   &typeSubst{pkg: pkg, named: make(map[*types2.Named]*types2.Named), aliases: make(map[*types2.Alias]*types2.Alias), tparams: make(map[*types2.TypeParam]*types2.TypeParam)},
		nodes: make(map[types2.Type]bool), deps: make(map[types2.Type]map[types2.Type]bool), affected: make(map[types2.Type]bool),
	}
}

// references visits all structural positions, including private fields and
// generic bounds. Local nominals are edges, not recursively expanded here.
func (g *overlayGraph) references(t types2.Type, refs map[types2.Type]bool, seen map[types2.Type]bool) {
	if t == nil || seen[t] {
		return
	}
	seen[t] = true
	switch t := t.(type) {
	case *types2.Named:
		if t.Obj().Pkg() == g.orig {
			n := t.Origin()
			refs[n] = true
			g.nodes[n] = true
		}
		for i := range t.TypeArgs().Len() {
			g.references(t.TypeArgs().At(i), refs, seen)
		}
	case *types2.Alias:
		if t.Obj().Pkg() == g.orig {
			a := t.Origin()
			refs[a] = true
			g.nodes[a] = true
		}
		g.references(t.Rhs(), refs, seen)
		for i := range t.TypeArgs().Len() {
			g.references(t.TypeArgs().At(i), refs, seen)
		}
	case *types2.TypeParam:
		refs[t] = true
		g.references(t.Constraint(), refs, seen)
	case *types2.Pointer:
		g.references(t.Elem(), refs, seen)
	case *types2.Chan:
		g.references(t.Elem(), refs, seen)
	case *types2.Slice:
		g.references(t.Elem(), refs, seen)
	case *types2.Array:
		g.references(t.Elem(), refs, seen)
	case *types2.Map:
		g.references(t.Key(), refs, seen)
		g.references(t.Elem(), refs, seen)
	case *types2.Option:
		g.references(t.Elem, refs, seen)
	case *types2.ConstView:
		g.references(t.Elem, refs, seen)
	case *types2.Struct:
		for i := range t.NumFields() {
			g.references(t.Field(i).Type(), refs, seen)
		}
	case *types2.Tuple:
		if t != nil {
			for i := range t.Len() {
				g.references(t.At(i).Type(), refs, seen)
			}
		}
	case *types2.Signature:
		if t.Recv() != nil {
			g.references(t.Recv().Type(), refs, seen)
		}
		g.references(t.Params(), refs, seen)
		g.references(t.Results(), refs, seen)
		for _, list := range []*types2.TypeParamList{t.TypeParams(), t.RecvTypeParams()} {
			for i := range list.Len() {
				g.references(list.At(i).Constraint(), refs, seen)
			}
		}
	case *types2.Interface:
		for i := range t.NumExplicitMethods() {
			g.references(t.ExplicitMethod(i).Type(), refs, seen)
		}
		for i := range t.NumEmbeddeds() {
			g.references(t.EmbeddedType(i), refs, seen)
		}
	case *types2.Union:
		for i := range t.Len() {
			g.references(t.Term(i).Type(), refs, seen)
		}
	}
}

// route records declaration-relative suffixes when a public path crosses a
// local nominal. It follows only the container explicitly named by a step.
func (g *overlayGraph) route(t types2.Type, steps []string, q qualKind) {
	if t == nil || len(steps) == 0 {
		return
	}
	switch n := t.(type) {
	case *types2.Named:
		if n.Obj().Pkg() != g.orig {
			return
		}
		name := n.Origin().Obj().Name()
		p := qualPath{path: joinPath(steps), qual: q}
		if !slices.Contains(g.paths[name], p) {
			g.paths[name] = append(g.paths[name], p)
		}
		u := n.Underlying()
		for _, outer := range g.paths[name] {
			if strings.HasPrefix(p.path, outer.path+".") {
				u = wrapAtQual(g.sub.pkg, u, strings.Split(outer.path, "."), outer.qual)
			}
		}
		g.route(u, steps, q)
		return
	case *types2.Alias:
		g.route(n.Rhs(), steps, q)
		return
	}
	head, rest := steps[0], steps[1:]
	switch u := t.(type) {
	case *types2.Option:
		if head == "E" {
			g.route(u.Elem, rest, q)
		}
	case *types2.ConstView:
		if head == "E" {
			g.route(u.Elem, rest, q)
		}
	case *types2.Pointer:
		if head == "D" {
			g.route(u.Elem(), rest, q)
		}
	case *types2.Chan:
		if head == "D" {
			g.route(u.Elem(), rest, q)
		}
	case *types2.Slice:
		if head == "E" {
			g.route(u.Elem(), rest, q)
		}
	case *types2.Array:
		if head == "E" {
			g.route(u.Elem(), rest, q)
		}
	case *types2.Map:
		if head == "K" {
			g.route(u.Key(), rest, q)
		}
		if head == "E" {
			g.route(u.Elem(), rest, q)
		}
	case *types2.Struct:
		if len(head) < 2 || head[0] != 'F' {
			return
		}
		for i := range u.NumFields() {
			if f := u.Field(i); f.Name() == head[1:] {
				g.route(f.Type(), rest, q)
				return
			}
		}
	case *types2.Interface:
		if len(head) < 2 || head[0] != 'M' {
			return
		}
		for i := range u.NumExplicitMethods() {
			if m := u.ExplicitMethod(i); m.Name() == head[1:] {
				g.route(m.Type(), rest, q)
				return
			}
		}
	case *types2.Signature:
		if len(head) < 2 {
			return
		}
		var tuple *types2.Tuple
		switch head[0] {
		case 'P':
			tuple = u.Params()
		case 'R':
			tuple = u.Results()
		default:
			return
		}
		var index int
		if _, err := fmt.Sscanf(head[1:], "%d", &index); err != nil {
			return
		}
		if index >= 0 && index < tuple.Len() {
			g.route(tuple.At(index).Type(), rest, q)
		}
	}
}

func (g *overlayGraph) routeObject(object string, t types2.Type) {
	// Replay outer qualifiers before routing deeper E steps. This models the
	// restored view without changing the lowered input graph.
	for _, p := range slices.Clone(g.paths[object]) {
		steps := strings.Split(p.path, ".")
		g.route(t, steps, p.qual)
		t = wrapAtQual(g.sub.pkg, t, steps, p.qual)
	}
}

func (g *overlayGraph) prepare() {
	for _, name := range g.orig.Scope().Names() {
		obj := g.orig.Scope().Lookup(name)
		g.routeObject(name, obj.Type())
		if n, ok := obj.Type().(*types2.Named); ok {
			for i := range n.NumMethods() {
				m := n.Method(i)
				g.routeObject(methodObject(name, m.Type().(*types2.Signature).Recv(), m.Name()), m.Type())
			}
		}
	}
	for _, name := range g.orig.Scope().Names() {
		g.references(g.orig.Scope().Lookup(name).Type(), make(map[types2.Type]bool), make(map[types2.Type]bool))
	}
	for {
		changed := false
		for node := range g.nodes {
			if _, ok := g.deps[node]; ok {
				continue
			}
			changed = true
			refs := make(map[types2.Type]bool)
			g.deps[node] = refs
			seen := make(map[types2.Type]bool)
			var name string
			switch n := node.(type) {
			case *types2.Named:
				name = n.Obj().Name()
				g.references(n.Underlying(), refs, seen)
				for i := range n.NumMethods() {
					m := n.Method(i)
					g.references(m.Type(), refs, seen)
					if len(g.paths[methodObject(name, m.Type().(*types2.Signature).Recv(), m.Name())]) != 0 {
						g.affected[node] = true
					}
				}
				for i := range n.TypeParams().Len() {
					g.references(n.TypeParams().At(i).Constraint(), refs, seen)
				}
			case *types2.Alias:
				name = n.Obj().Name()
				g.references(n.Rhs(), refs, seen)
				for i := range n.TypeParams().Len() {
					g.references(n.TypeParams().At(i).Constraint(), refs, seen)
				}
			}
			if len(g.paths[name]) != 0 {
				g.affected[node] = true
			}
		}
		if !changed {
			break
		}
	}
	for {
		changed := false
		for node, refs := range g.deps {
			if g.affected[node] {
				continue
			}
			for ref := range refs {
				if g.affected[ref] {
					g.affected[node] = true
					changed = true
					break
				}
			}
		}
		if !changed {
			break
		}
	}
	for name, paths := range g.paths {
		slices.SortStableFunc(paths, func(a, b qualPath) int {
			if d := strings.Count(a.path, ".") - strings.Count(b.path, "."); d != 0 {
				return d
			}
			return strings.Compare(a.path, b.path)
		})
		g.paths[name] = paths
	}
	// Phase one: all defined-type shells and all their parameter identities.
	for node := range g.affected {
		if n, ok := node.(*types2.Named); ok {
			obj := n.Obj()
			g.sub.named[n] = types2.NewNamed(types2.NewTypeName(obj.Pos(), g.sub.pkg, obj.Name(), nil), nil, nil)
		}
	}
	for old, shell := range g.sub.named {
		params := typeParamSlice(old.TypeParams())
		news := make([]*types2.TypeParam, len(params))
		for i, p := range params {
			news[i] = types2.NewTypeParam(types2.NewTypeName(p.Obj().Pos(), g.sub.pkg, p.Obj().Name(), nil), nil)
			g.sub.tparams[p] = news[i]
		}
		shell.SetTypeParams(news)
	}
	g.sub.graph = g
	// Aliases are acyclic RHS expressions; lazy construction resolves their
	// dependency order while references through defined types use the shells.
	for node := range g.affected {
		if a, ok := node.(*types2.Alias); ok {
			g.alias(a)
		}
	}
	for old := range g.sub.named {
		for i := range old.TypeParams().Len() {
			p := old.TypeParams().At(i)
			g.sub.tparams[p].SetConstraint(substType(g.sub, p.Constraint()))
		}
	}
	// Phase two: fill every shell before publishing any package objects.
	for old, shell := range g.sub.named {
		u := substType(g.sub, old.Underlying())
		for _, p := range g.paths[old.Obj().Name()] {
			u = wrapAtQual(g.sub.pkg, u, strings.Split(p.path, "."), p.qual)
		}
		shell.SetUnderlying(u)
	}
	for old, shell := range g.sub.named {
		for i := range old.NumMethods() {
			m := old.Method(i)
			sig := substSignature(g.sub, m.Type().(*types2.Signature))
			for _, p := range g.paths[methodObject(old.Obj().Name(), m.Type().(*types2.Signature).Recv(), m.Name())] {
				sig = wrapAtQual(g.sub.pkg, sig, strings.Split(p.path, "."), p.qual).(*types2.Signature)
			}
			shell.AddMethod(types2.NewFunc(m.Pos(), g.sub.pkg, m.Name(), sig))
		}
	}
}

func (g *overlayGraph) alias(a *types2.Alias) *types2.Alias {
	if n := g.sub.aliases[a]; n != nil {
		return n
	}
	inner, params := freshOwnTypeParams(g.sub, a.TypeParams())
	rhs := substType(inner, a.Rhs())
	for _, p := range g.paths[a.Obj().Name()] {
		rhs = wrapAtQual(g.sub.pkg, rhs, strings.Split(p.path, "."), p.qual)
	}
	obj := a.Obj()
	n := types2.NewAlias(types2.NewTypeName(obj.Pos(), g.sub.pkg, obj.Name(), nil), rhs)
	n.SetTypeParams(params)
	g.sub.aliases[a] = n
	return n
}

// typeSubst remaps rebuilt declarations to their overlay copies: tparams
// maps an old *TypeParam to its fresh copy, and named maps an old
// *Named origin to its shared shell. Aliases and graph are shared by all
// parameter scopes; only the tparams environment is extended.
type typeSubst struct {
	pkg     *types2.Package
	tparams map[*types2.TypeParam]*types2.TypeParam
	named   map[*types2.Named]*types2.Named
	aliases map[*types2.Alias]*types2.Alias
	graph   *overlayGraph
}

// typeParamSlice converts a type parameter list to a slice, tolerating nil.
func typeParamSlice(l *types2.TypeParamList) []*types2.TypeParam {
	if l == nil || l.Len() == 0 {
		return nil
	}
	out := make([]*types2.TypeParam, l.Len())
	for i := range l.Len() {
		out[i] = l.At(i)
	}
	return out
}

// freshTypeParams copies olds into sub's package with constraints
// substituted through the extended map, so mutually recursive bounds
// resolve. Each copy binds exactly once, at its owner.
func freshTypeParams(sub *typeSubst, olds []*types2.TypeParam) ([]*types2.TypeParam, map[*types2.TypeParam]*types2.TypeParam) {
	news := make([]*types2.TypeParam, len(olds))
	delta := make(map[*types2.TypeParam]*types2.TypeParam, len(olds))
	for i, o := range olds {
		obj := types2.NewTypeName(o.Obj().Pos(), sub.pkg, o.Obj().Name(), nil)
		news[i] = types2.NewTypeParam(obj, nil)
		delta[o] = news[i]
	}
	ext := make(map[*types2.TypeParam]*types2.TypeParam, len(sub.tparams)+len(delta))
	maps.Copy(ext, sub.tparams)
	maps.Copy(ext, delta)
	inner := &typeSubst{pkg: sub.pkg, tparams: ext, named: sub.named, aliases: sub.aliases, graph: sub.graph}
	for i, o := range olds {
		if b := o.Constraint(); b != nil {
			news[i].SetConstraint(substType(inner, b))
		}
	}
	return news, ext
}

// substType remaps t into the overlay copy: old type parameters become
// their fresh copies, references to the rebuilt named become its shell,
// and instantiated generics re-instantiate with substituted arguments.
// Unmapped and unchanged positions share the input, so non-generic
// declarations keep their identity.
func substType(sub *typeSubst, t types2.Type) types2.Type {
	if t == nil {
		return nil
	}
	if p, ok := t.(*types2.TypeParam); ok {
		if n, ok := sub.tparams[p]; ok {
			return n
		}
		return t
	}
	if n, ok := t.(*types2.Named); ok {
		if nn, ok := sub.named[n]; ok {
			return nn
		}
		args := n.TypeArgs()
		if args == nil || args.Len() == 0 {
			return t
		}
		targs := make([]types2.Type, args.Len())
		changed := sub.named[n.Origin()] != nil
		for i := range args.Len() {
			a := substType(sub, args.At(i))
			targs[i] = a
			if a != args.At(i) {
				changed = true
			}
		}
		if !changed {
			return t
		}
		origin := n.Origin()
		if nn, ok := sub.named[origin]; ok {
			origin = nn
		}
		inst, err := types2.Instantiate(nil, origin, targs, false)
		if err != nil {
			panic("overlay named instantiation invariant: " + err.Error())
		}
		return inst
	}
	switch u := t.(type) {
	case *types2.Basic:
		return t
	case *types2.Array:
		e := substType(sub, u.Elem())
		if e == u.Elem() {
			return t
		}
		return types2.NewArray(e, u.Len())
	case *types2.Slice:
		e := substType(sub, u.Elem())
		if e == u.Elem() {
			return t
		}
		return types2.NewSlice(e)
	case *types2.Map:
		k := substType(sub, u.Key())
		e := substType(sub, u.Elem())
		if k == u.Key() && e == u.Elem() {
			return t
		}
		return types2.NewMap(k, e)
	case *types2.Pointer:
		e := substType(sub, u.Elem())
		if e == u.Elem() {
			return t
		}
		return types2.NewPointer(e)
	case *types2.Chan:
		e := substType(sub, u.Elem())
		if e == u.Elem() {
			return t
		}
		return types2.NewChan(u.Dir(), e)
	case *types2.Option:
		e := substType(sub, u.Elem)
		if e == u.Elem {
			return t
		}
		return &types2.Option{Elem: e}
	case *types2.ConstView:
		e := substType(sub, u.Elem)
		if e == u.Elem {
			return t
		}
		return &types2.ConstView{Elem: e, ConstMap: u.ConstMap}
	case *types2.Struct:
		fields := make([]*types2.Var, u.NumFields())
		tags := make([]string, u.NumFields())
		changed := false
		for i := range u.NumFields() {
			f := u.Field(i)
			tags[i] = u.Tag(i)
			e := substType(sub, f.Type())
			if e == f.Type() {
				fields[i] = f
				continue
			}
			nf := types2.NewField(f.Pos(), sub.pkg, f.Name(), e, f.Embedded())
			fields[i] = nf
			changed = true
		}
		if !changed {
			return t
		}
		return types2.NewStruct(fields, tags)
	case *types2.Signature:
		return substSignature(sub, u)
	case *types2.Interface:
		methods := make([]*types2.Func, u.NumExplicitMethods())
		changed := false
		for i := range u.NumExplicitMethods() {
			m := u.ExplicitMethod(i)
			sig, ok := m.Type().(*types2.Signature)
			if !ok {
				methods[i] = m
				continue
			}
			ns := substSignature(sub, sig)
			if ns == sig {
				methods[i] = m
				continue
			}
			methods[i] = types2.NewFunc(m.Pos(), sub.pkg, m.Name(), ns)
			changed = true
		}
		embeddeds := make([]types2.Type, u.NumEmbeddeds())
		for i := range u.NumEmbeddeds() {
			e := substType(sub, u.EmbeddedType(i))
			embeddeds[i] = e
			if e != u.EmbeddedType(i) {
				changed = true
			}
		}
		if !changed {
			return t
		}
		return types2.NewInterfaceType(methods, embeddeds)
	case *types2.Union:
		terms := make([]*types2.Term, u.Len())
		changed := false
		for i := range u.Len() {
			tm := u.Term(i)
			e := substType(sub, tm.Type())
			if e == tm.Type() {
				terms[i] = tm
				continue
			}
			terms[i] = types2.NewTerm(tm.Tilde(), e)
			changed = true
		}
		if !changed || len(terms) == 0 {
			return t
		}
		return types2.NewUnion(terms)
	case *types2.Alias:
		origin := u.Origin()
		mapped := sub.aliases[origin]
		if mapped == nil && sub.graph != nil && sub.graph.affected[origin] {
			mapped = sub.graph.alias(origin)
		}
		if mapped == nil {
			mapped = origin
		}
		args := u.TypeArgs()
		if args.Len() == 0 {
			return mapped
		}
		targs := make([]types2.Type, args.Len())
		changed := mapped != origin
		for i := range args.Len() {
			targs[i] = substType(sub, args.At(i))
			changed = changed || targs[i] != args.At(i)
		}
		if !changed {
			return t
		}
		inst, err := types2.Instantiate(nil, mapped, targs, false)
		if err != nil {
			panic("overlay alias instantiation invariant: " + err.Error())
		}
		return inst
	default:
		return t
	}
}

// substSignature remaps sig into the overlay copy, giving signatures
// that own type parameters fresh copies scoped to the new signature.
// Each copy binds once, at the returned signature.
func substSignature(sub *typeSubst, sig *types2.Signature) *types2.Signature {
	if sub.graph != nil {
		refs := make(map[types2.Type]bool)
		sub.graph.references(sig, refs, make(map[types2.Type]bool))
		changed := false
		for ref := range refs {
			if sub.graph.affected[ref] {
				changed = true
				break
			}
			if p, ok := ref.(*types2.TypeParam); ok && sub.tparams[p] != nil {
				changed = true
				break
			}
		}
		if !changed {
			return sig
		}
	}
	inner := sub
	var newT, newR []*types2.TypeParam
	if tp := sig.TypeParams(); tp != nil && tp.Len() > 0 {
		var ext map[*types2.TypeParam]*types2.TypeParam
		newT, ext = freshTypeParams(sub, typeParamSlice(tp))
		inner = &typeSubst{pkg: sub.pkg, tparams: ext, named: sub.named, aliases: sub.aliases, graph: sub.graph}
	}
	if rp := sig.RecvTypeParams(); rp != nil && rp.Len() > 0 {
		var ext map[*types2.TypeParam]*types2.TypeParam
		newR, ext = freshTypeParams(inner, typeParamSlice(rp))
		inner = &typeSubst{pkg: sub.pkg, tparams: ext, named: sub.named, aliases: sub.aliases, graph: sub.graph}
	}
	changed := newT != nil || newR != nil
	var nrecv *types2.Var
	if r := sig.Recv(); r != nil {
		rt := substType(inner, r.Type())
		if rt != r.Type() {
			changed = true
		}
		nrecv = types2.NewVar(r.Pos(), sub.pkg, r.Name(), rt)
		nrecv.SetKind(r.Kind())
	}
	params, paramsChanged := substTuple(inner, sig.Params())
	results, resultsChanged := substTuple(inner, sig.Results())
	if !changed && !paramsChanged && !resultsChanged {
		return sig
	}
	return types2.NewSignatureType(nrecv, newR, newT, params, results, sig.Variadic())
}

// substTuple remaps a tuple, sharing it when no position changes.
func substTuple(sub *typeSubst, tup *types2.Tuple) (*types2.Tuple, bool) {
	if tup == nil {
		return nil, false
	}
	vars := make([]*types2.Var, tup.Len())
	changed := false
	for i := range tup.Len() {
		v := tup.At(i)
		e := substType(sub, v.Type())
		if e == v.Type() {
			vars[i] = v
			continue
		}
		nv := types2.NewVar(v.Pos(), sub.pkg, v.Name(), e)
		nv.SetKind(v.Kind())
		vars[i] = nv
		changed = true
	}
	if !changed {
		return tup, false
	}
	return types2.NewTuple(vars...), true
}

// cloneInterfaceMethods rebuilds the explicit methods named by paths,
// preserving already-remapped generic parameter ownership.
func cloneInterfaceMethods(pkg *types2.Package, u *types2.Interface, paths []qualPath) *types2.Interface {
	byMethod := map[string][]qualPath{}
	for _, p := range paths {
		steps := strings.Split(p.path, ".")
		if len(steps) < 2 || len(steps[0]) < 2 || steps[0][0] != 'M' {
			continue
		}
		rest := strings.Join(steps[1:], ".")
		name := steps[0][1:]
		byMethod[name] = append(byMethod[name], qualPath{path: rest, qual: p.qual})
	}
	if len(byMethod) == 0 {
		return u
	}
	methods := make([]*types2.Func, u.NumExplicitMethods())
	for i := range u.NumExplicitMethods() {
		m := u.ExplicitMethod(i)
		methods[i] = m
		sig, ok := m.Type().(*types2.Signature)
		if !ok {
			continue
		}
		qp := byMethod[m.Name()]
		if len(qp) == 0 {
			continue
		}
		nsig := sig
		for _, p := range qp {
			nsig = wrapSignaturePos(pkg, nsig, strings.Split(p.path, "."), p.qual).(*types2.Signature)
		}
		methods[i] = types2.NewFunc(m.Pos(), pkg, m.Name(), nsig)
	}
	embeddeds := make([]types2.Type, u.NumEmbeddeds())
	for i := range u.NumEmbeddeds() {
		embeddeds[i] = u.EmbeddedType(i)
	}
	return types2.NewInterfaceType(methods, embeddeds)
}

// hasOwnTypeParams reports whether sig owns type parameters.
func hasOwnTypeParams(sig *types2.Signature) bool {
	if tp := sig.TypeParams(); tp != nil && tp.Len() > 0 {
		return true
	}
	if rp := sig.RecvTypeParams(); rp != nil && rp.Len() > 0 {
		return true
	}
	return false
}

// applyQual wraps t in the qualifier kind: Option for qualOption,
// ConstView for qualConst. An already-qualified type is returned
// unchanged, so repeated paths stay idempotent.
func applyQual(t types2.Type, q qualKind) types2.Type {
	if q == qualConst {
		return toConstView(t)
	}
	return toOption(t)
}

// wrapAtQual converts t at the remaining steps for one qualifier kind:
// empty steps wrap the position itself; E steps descend into
// qualifier/slice/map elems; K steps descend into map keys; F steps
// descend into anonymous struct fields; M steps descend into anonymous
// interface methods; P and R steps descend into signature positions.
// D consumes exactly one pointer or channel container. Named positions
// stop: named types convert whole at their declaration site, which also
// preserves recursive references. Generic signature positions receive
// fresh parameters bound exactly once at the rebuilt signature.
func wrapAtQual(pkg *types2.Package, t types2.Type, steps []string, q qualKind) types2.Type {
	if len(steps) == 0 {
		return applyQual(t, q)
	}
	if t == nil {
		return t
	}
	if o := optAsOption(t); o != nil {
		if len(steps) == 0 {
			return applyQual(t, q)
		}
		if steps[0] != "E" {
			return t
		}
		elem := wrapAtQual(pkg, o.Elem, steps[1:], q)
		if elem == o.Elem {
			return t
		}
		return &types2.Option{Elem: elem}
	}
	if c := constAsView(t); c != nil {
		if len(steps) == 0 {
			return applyQual(t, q)
		}
		if steps[0] != "E" {
			return t
		}
		elem := wrapAtQual(pkg, c.Elem, steps[1:], q)
		if elem == c.Elem {
			return t
		}
		return &types2.ConstView{Elem: elem, ConstMap: c.ConstMap}
	}
	u := types2.Unalias(t)
	switch u := u.(type) {
	case *types2.Slice:
		if steps[0] != "E" {
			return t
		}
		return types2.NewSlice(wrapAtQual(pkg, u.Elem(), steps[1:], q))
	case *types2.Array:
		if steps[0] != "E" {
			return t
		}
		return types2.NewArray(wrapAtQual(pkg, u.Elem(), steps[1:], q), u.Len())
	case *types2.Map:
		if steps[0] == "K" {
			return types2.NewMap(wrapAtQual(pkg, u.Key(), steps[1:], q), u.Elem())
		}
		if steps[0] == "E" {
			return types2.NewMap(u.Key(), wrapAtQual(pkg, u.Elem(), steps[1:], q))
		}
		return t
	case *types2.Pointer:
		if steps[0] != "D" {
			return t
		}
		elem := wrapAtQual(pkg, u.Elem(), steps[1:], q)
		if elem == u.Elem() {
			return t
		}
		return types2.NewPointer(elem)
	case *types2.Chan:
		if steps[0] != "D" {
			return t
		}
		elem := wrapAtQual(pkg, u.Elem(), steps[1:], q)
		if elem == u.Elem() {
			return t
		}
		return types2.NewChan(u.Dir(), elem)
	case *types2.Struct:
		if len(steps[0]) < 2 || steps[0][0] != 'F' {
			return t
		}
		return wrapStructField(pkg, u, steps[0][1:], steps[1:], q)
	case *types2.Signature:
		return wrapSignaturePos(pkg, u, steps, q)
	case *types2.Interface:
		if len(steps[0]) < 2 || steps[0][0] != 'M' {
			return t
		}
		return cloneInterfaceMethods(pkg, u, []qualPath{{path: joinPath(steps), qual: q}})
	case *types2.Named:
		return t
	default:
		return t
	}
}

// wrapStructField converts the named field of anonymous struct u at the
// remaining steps, sharing every other field. With no change it returns
// u, so declarations without matching facts keep their identity.
func wrapStructField(pkg *types2.Package, u *types2.Struct, name string, rest []string, q qualKind) types2.Type {
	idx := -1
	for i := range u.NumFields() {
		if f := u.Field(i); f != nil && f.Name() == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return u
	}
	fields := make([]*types2.Var, u.NumFields())
	tags := make([]string, u.NumFields())
	changed := false
	for i := range u.NumFields() {
		f := u.Field(i)
		tags[i] = u.Tag(i)
		if i != idx {
			fields[i] = f
			continue
		}
		nt := wrapAtQual(pkg, f.Type(), rest, q)
		if nt == f.Type() {
			fields[i] = f
			continue
		}
		nf := types2.NewField(f.Pos(), pkg, f.Name(), nt, f.Embedded())
		fields[i] = nf
		changed = true
	}
	if !changed {
		return u
	}
	return types2.NewStruct(fields, tags)
}

// wrapSignaturePos converts the indexed param or result position of
// signature u at the remaining steps, sharing every other position.
// Generic signatures use wrapGenericSignaturePos to preserve ownership.
func wrapSignaturePos(pkg *types2.Package, u *types2.Signature, steps []string, q qualKind) types2.Type {
	if len(steps[0]) < 2 || (steps[0][0] != 'P' && steps[0][0] != 'R') {
		return u
	}
	if hasOwnTypeParams(u) {
		return wrapGenericSignaturePos(pkg, u, steps, q)
	}
	var idx int
	if _, err := fmt.Sscanf(steps[0][1:], "%d", &idx); err != nil {
		return u
	}
	var tup *types2.Tuple
	if steps[0][0] == 'P' {
		tup = u.Params()
	} else {
		tup = u.Results()
	}
	if tup == nil || idx < 0 || idx >= tup.Len() {
		return u
	}
	vars := make([]*types2.Var, tup.Len())
	changed := false
	for i := range tup.Len() {
		v := tup.At(i)
		if i != idx {
			vars[i] = v
			continue
		}
		nt := wrapAtQual(pkg, v.Type(), steps[1:], q)
		if nt == v.Type() {
			vars[i] = v
			continue
		}
		nv := types2.NewVar(v.Pos(), pkg, v.Name(), nt)
		nv.SetKind(v.Kind())
		vars[i] = nv
		changed = true
	}
	if !changed {
		return u
	}
	ntup := types2.NewTuple(vars...)
	var params, results *types2.Tuple
	if steps[0][0] == 'P' {
		params, results = ntup, u.Results()
	} else {
		params, results = u.Params(), ntup
	}
	return types2.NewSignatureType(u.Recv(), nil, nil, params, results, u.Variadic())
}

// wrapGenericSignaturePos converts the indexed param or result position
// of a signature that owns type parameters. It fuses substitution and
// wrapping into one construction: fresh copies of the owned parameters
// with bounds substituted, every position remapped through them, and
// the qualifier applied, so the single NewSignatureType call binds each
// copy exactly once. A receiver without a type is refused rather than
// built malformed.
func wrapGenericSignaturePos(pkg *types2.Package, u *types2.Signature, steps []string, q qualKind) types2.Type {
	var idx int
	if _, err := fmt.Sscanf(steps[0][1:], "%d", &idx); err != nil {
		return u
	}
	base := &typeSubst{pkg: pkg}
	inner, newT := freshOwnTypeParams(base, u.TypeParams())
	inner, newR := freshOwnTypeParams(inner, u.RecvTypeParams())
	full := inner
	var nrecv *types2.Var
	if r := u.Recv(); r != nil {
		rt := substType(full, r.Type())
		nrecv = types2.NewVar(r.Pos(), pkg, r.Name(), rt)
		nrecv.SetKind(r.Kind())
	} else if newR != nil {
		return u
	}
	params, _ := substTuple(full, u.Params())
	results, _ := substTuple(full, u.Results())
	var tup *types2.Tuple
	if steps[0][0] == 'P' {
		tup = params
	} else {
		tup = results
	}
	if tup == nil || idx < 0 || idx >= tup.Len() {
		return u
	}
	v := tup.At(idx)
	nt := wrapAtQual(pkg, v.Type(), steps[1:], q)
	if nt != v.Type() {
		vars := make([]*types2.Var, tup.Len())
		for i := range tup.Len() {
			vars[i] = tup.At(i)
		}
		nv := types2.NewVar(v.Pos(), pkg, v.Name(), nt)
		nv.SetKind(v.Kind())
		vars[idx] = nv
		ntup := types2.NewTuple(vars...)
		if steps[0][0] == 'P' {
			params = ntup
		} else {
			results = ntup
		}
	}
	return types2.NewSignatureType(nrecv, newR, newT, params, results, u.Variadic())
}

// freshOwnTypeParams copies an owned parameter list, or returns nil when
// the list is absent.
func freshOwnTypeParams(sub *typeSubst, l *types2.TypeParamList) (*typeSubst, []*types2.TypeParam) {
	if l == nil || l.Len() == 0 {
		return sub, nil
	}
	news, ext := freshTypeParams(sub, typeParamSlice(l))
	return &typeSubst{pkg: sub.pkg, tparams: ext, named: sub.named, aliases: sub.aliases, graph: sub.graph}, news
}

// toConstView converts a lowered type to a const view. A restored map
// keeps its const-map mark; ordinary const views have no such mark.
func toConstView(t types2.Type) types2.Type {
	if t == nil {
		return &types2.ConstView{}
	}
	if c := constAsView(t); c != nil {
		return t
	}
	if _, ok := types2.Unalias(t).(*types2.Map); ok {
		return &types2.ConstView{Elem: t, ConstMap: true}
	}
	return &types2.ConstView{Elem: t}
}

// toOption converts a lowered type to Option: rt.Option[T] becomes
// Option with Elem T; any other type wraps as Option.
func toOption(t types2.Type) types2.Type {
	if t == nil {
		return &types2.Option{}
	}
	if o, ok := types2.Unalias(t).(*types2.Option); ok {
		return o
	}
	if n, ok := types2.Unalias(t).(*types2.Named); ok {
		if obj := n.Obj(); obj != nil && obj.Name() == "Option" && obj.Pkg() != nil && obj.Pkg().Path() == "logos/rt" {
			if targs := n.TypeArgs(); targs != nil && targs.Len() == 1 {
				return &types2.Option{Elem: targs.At(0)}
			}
		}
	}
	return &types2.Option{Elem: t}
}
