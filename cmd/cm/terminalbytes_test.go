package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCommandLayerWritesNoEscapeSequences enforces the invariant a rendering bug was traced to: the
// bytes that reach a terminal are constructed in internal/client and nowhere else.
//
// The command layer used to build one itself. `fmt.Fprintf(os.Stdout, "\x1b]2;%s\x07", ...)` set the
// window title from a metadata callback, which put it outside internal/client.TTY and outside any
// ordering with the session's output. It landed between the two halves of a split escape sequence and
// the remainder printed as text on screen, and the cause was three rounds of instrumentation away from
// the symptom because every capture taken inside cm missed a writer that bypassed cm's own abstraction.
//
// A test rather than a comment asking nicely, because the reason it happened is that writing an escape
// sequence here is easy and looks harmless. Anything cm says to a terminal needs to know whether the
// stream is mid-sequence, only internal/client knows that, so only internal/client writes it. This
// layer states policy: see the SetTitle option.
//
// Scoped to escape *literals* rather than to os.Stdout, which this package writes plain CLI output to
// all day long. Prose may say ESC in words; what may not appear is a byte a terminal would act on.
func TestCommandLayerWritesNoEscapeSequences(t *testing.T) {
	// forbidden are the ways an escape byte gets into Go source: the escapes Go accepts, and the
	// byte itself, which a paste can leave behind.
	forbidden := []string{`\x1b`, `\033`, `\u001b`, "\x1b"}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		// Parsed rather than grepped, so the check looks at string literals and not at comments. A
		// comment describing the bug this prevents would otherwise trip it, which is how a rule like this
		// gets deleted instead of obeyed.
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		checked++
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			for _, bad := range forbidden {
				if strings.Contains(lit.Value, bad) {
					t.Errorf("%s:%d: escape sequence %s in a string literal here.\n"+
						"Bytes for a terminal are built in internal/client, which is the only place that "+
						"knows whether the session's output is mid-sequence. Writing one here is what put a "+
						"window title inside a program's SGR and printed its tail on screen. Add an option "+
						"to client.Options instead, as SetTitle does.",
						filepath.Join("cmd/cm", name), fset.Position(lit.Pos()).Line, bad)
					return false
				}
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("no source files were checked, so this test proves nothing")
	}
}
