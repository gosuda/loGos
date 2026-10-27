// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package lsp

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// TestProxyCloseUnsavedDocumentRecompilesRetainedPackage verifies PRRT_kwDOVOeymc6vYBsI:
// When an unsaved .lgo document is closed while a sibling document in the same package
// remains open, the proxy must mark the retained package dirty, recompile against disk,
// update gopls with the restored disk lowering, and ensure subsequent queries from sibling
// and dependent packages reflect the on-disk source rather than discarded in-memory edits.
func TestProxyCloseUnsavedDocumentRecompilesRetainedPackage(t *testing.T) {
	const doc1Disk = `package a

import "os"

func Read(path string) (string, error) {
	data := os.ReadFile(path)?
	return string(data), nil
}

func Helper() int {
	return 42
}
`
	const doc2Disk = `package a

func Sibling() int {
	return Helper()
}
`
	const depDisk = `package b

import "example.com/m/a"

func Dependent() int {
	return a.Helper()
}
`

	lowDisk, _, ok := lowerQuestion([]byte(doc1Disk))
	if !ok {
		t.Fatal("lowering doc1Disk failed")
	}

	posHelperDisk := findPos(t, doc1Disk, "Helper", 0)
	rangeHelperDisk := span(posHelperDisk, len("Helper"))

	posLowHelperDisk := findPos(t, string(lowDisk), "Helper", 0)
	rangeLowHelperDisk := span(posLowHelperDisk, len("Helper"))

	f := newFixture(t, map[string]string{
		"a/doc1.lgo": doc1Disk,
		"a/doc2.lgo": doc2Disk,
		"b/b.lgo":    depDisk,
	}, fixtureOptions{
		Script: func(root string) fakeScript {
			doc1Virt := pathURI(virtualPath(filepath.Join(root, "a", "doc1.lgo")))
			return fakeScript{
				Responses: map[string]json.RawMessage{
					"textDocument/definition": toJSON(t, []any{
						map[string]any{
							"uri":   doc1Virt,
							"range": rangeLowHelperDisk,
						},
					}),
				},
			}
		},
		Tune: func(f *fixture, o *Options) {
			o.Debounce = -1
			f.comp.setClosure(f.path("b"), f.path("a"))
		},
	})

	// Open both same-package documents and the dependent unit document.
	f.open("a/doc1.lgo", 1)
	f.open("a/doc2.lgo", 1)
	f.open("b/b.lgo", 1)

	// Apply an unsaved edit to doc1.lgo that shifts lines downward.
	const prefix = "\n\n\n\n\n\n\n\n\n\n" // 10 lines
	unsavedDoc1 := prefix + doc1Disk
	f.client.notify("textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": f.uri("a/doc1.lgo"), "version": 2},
		"contentChanges": []any{map[string]any{"text": unsavedDoc1}},
	})

	// Synchronize with the proxy via an explicit request to compile the unsaved edit.
	posUnsaved := Position{Line: posHelperDisk.Line + 10, Character: posHelperDisk.Character}
	f.client.call("textDocument/hover", positionParams(f.uri("a/doc1.lgo"), posUnsaved))

	// Close doc1.lgo without saving.
	// Sibling doc2.lgo remains open in package a, and b.lgo remains open in package b.
	f.client.notify("textDocument/didClose", map[string]any{
		"textDocument": map[string]any{"uri": f.uri("a/doc1.lgo")},
	})

	// Query definition of Helper from sibling doc2.lgo.
	// Because doc1 was closed without saving, its lowering and position mapping
	// must reflect the on-disk source, mapping Helper to rangeHelperDisk.
	siblingCallPos := findPos(t, doc2Disk, "Helper", 0)
	resSibling := f.client.call("textDocument/definition", positionParams(f.uri("a/doc2.lgo"), siblingCallPos))
	wantDef := []any{
		map[string]any{
			"uri":   f.uri("a/doc1.lgo"),
			"range": rangeHelperDisk,
		},
	}
	sameJSON(t, "sibling definition query reflects disk", resSibling, wantDef)

	// Query definition of Helper from dependent unit b.lgo.
	// The dependent unit must also reflect doc1 on disk.
	depCallPos := findPos(t, depDisk, "Helper", 0)
	resDep := f.client.call("textDocument/definition", positionParams(f.uri("b/b.lgo"), depCallPos))
	sameJSON(t, "dependent unit definition query reflects disk", resDep, wantDef)

	// Gopls must have received didChange for the virtual document restoring the disk content.
	var lastVirtText string
	for _, m := range f.receivedMethod("textDocument/didChange") {
		td, _ := m["textDocument"].(map[string]any)
		if td != nil && td["uri"] == f.virtURI("a/doc1.lgo") {
			changes, _ := m["contentChanges"].([]any)
			if len(changes) > 0 {
				c0, _ := changes[0].(map[string]any)
				if txt, ok := c0["text"].(string); ok {
					lastVirtText = txt
				}
			}
		}
	}
	if lastVirtText != string(lowDisk) {
		t.Errorf("gopls virtual document after close was not restored to disk text:\ngot:\n%s\nwant:\n%s", lastVirtText, string(lowDisk))
	}
}

// TestProxyFormattingRejectsLoweredGoForLogosAndPreservesGo verifies PRRT_kwDOVOeymc6vYBsT:
// Formatting requests on .lgo documents must not emit lowered Go statements, imports,
// or generated identifiers into source files (declining formatting with null, empty edits,
// or an error is allowed). Meanwhile, formatting on ordinary .go documents must remain
// supported and be forwarded to gopls unchanged.
func TestProxyFormattingRejectsLoweredGoForLogosAndPreservesGo(t *testing.T) {
	const origLgo = `package a

import "os"

func Read(path string) (string, error) {
	data := os.ReadFile(path)?
	return string(data), nil
}
`
	lowDisk, _, ok := lowerQuestion([]byte(origLgo))
	if !ok {
		t.Fatal("lowering origLgo failed")
	}

	t.Run("lgo formatting must not emit lowered Go", func(t *testing.T) {
		lowEdit := []any{
			map[string]any{
				"range":   Range{Start: Position{Line: 0, Character: 0}, End: Position{Line: 15, Character: 0}},
				"newText": string(lowDisk),
			},
		}
		f := newFixture(t, map[string]string{
			"a/a.lgo": origLgo,
		}, fixtureOptions{
			Script: func(root string) fakeScript {
				return fakeScript{
					Responses: map[string]json.RawMessage{
						"textDocument/formatting": toJSON(t, lowEdit),
					},
				}
			},
			Tune: func(f *fixture, o *Options) {
				o.Debounce = -1
			},
		})

		f.open("a/a.lgo", 1)

		msg := f.client.callMessage("textDocument/formatting", map[string]any{
			"textDocument": map[string]any{"uri": f.uri("a/a.lgo")},
			"options":      map[string]any{"tabSize": 8, "insertSpaces": false},
		})

		// A formatting request for a .lgo document must not emit edits containing
		// lowered Go text (such as generated error traces, synthetic sites, or lowered Go statements).
		// Declining formatting with an error, null result, or empty edit list is permitted.
		if len(msg.Error) == 0 && string(msg.Result) != "null" && string(msg.Result) != "[]" {
			var edits []struct {
				Range   Range  `json:"range"`
				NewText string `json:"newText"`
			}
			if err := json.Unmarshal(msg.Result, &edits); err != nil {
				t.Fatalf("unmarshaling formatting edits: %v", err)
			}
			for _, edit := range edits {
				if strings.Contains(edit.NewText, "_logos") || strings.Contains(edit.NewText, "_e :=") {
					t.Fatalf("formatting on .lgo emitted lowered Go edits:\n%s", edit.NewText)
				}
			}
		}
	})

	t.Run("go formatting is forwarded unchanged", func(t *testing.T) {
		const goSrc = "package g\n\nfunc Foo() {\n}\n"
		goEdits := []any{
			map[string]any{
				"range":   Range{Start: Position{Line: 0, Character: 0}, End: Position{Line: 3, Character: 0}},
				"newText": "package g\n\nfunc Foo() {\n\t// formatted\n}\n",
			},
		}
		f := newFixture(t, map[string]string{
			"g/g.go": goSrc,
		}, fixtureOptions{
			Script: func(root string) fakeScript {
				return fakeScript{
					Responses: map[string]json.RawMessage{
						"textDocument/formatting": toJSON(t, goEdits),
					},
				}
			},
			Tune: func(f *fixture, o *Options) {
				o.Debounce = -1
			},
		})

		f.open("g/g.go", 1)

		res := f.client.call("textDocument/formatting", map[string]any{
			"textDocument": map[string]any{"uri": f.uri("g/g.go")},
			"options":      map[string]any{"tabSize": 8, "insertSpaces": false},
		})
		sameJSON(t, "formatting on .go file", res, goEdits)

		reqs := f.receivedMethod("textDocument/formatting")
		if len(reqs) != 1 {
			t.Fatalf("gopls got %d formatting requests, want 1", len(reqs))
		}
		td, _ := reqs[0]["textDocument"].(map[string]any)
		if uri, _ := td["uri"].(string); uri != f.uri("g/g.go") {
			t.Errorf("forwarded formatting URI = %s, want %s", uri, f.uri("g/g.go"))
		}
	})
}
