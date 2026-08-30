package capability

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestAPeerReportingNothingIsUnknownRatherThanAbsent is the distinction the whole package exists for.
//
// An older peer sends no tokens, and protobuf hands the receiver an empty slice, which is exactly what a
// peer saying "I support none of these" would send. Folding those together is the bug: one is a fact to
// act on and the other is a reason to behave as cm always did. A server that read them the same way
// would treat every shim predating this mechanism as one that had refused, which today is every shim
// running.
func TestAPeerReportingNothingIsUnknownRatherThanAbsent(t *testing.T) {
	got := supportOfEach(Parse(nil))

	want := everyDeclared(Unknown)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("a peer that reported no tokens answers %v, want %v.\n"+
			"Absent here would mean the server concludes an old shim refused a capability it was never "+
			"asked about.", got, want)
	}
}

// The zero Set answers the same way, since a server holds one about a shim it has not asked yet.
func TestTheZeroSetIsUnknownRatherThanAbsent(t *testing.T) {
	var unasked Set

	got := supportOfEach(unasked)

	want := everyDeclared(Unknown)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the zero Set answers %v, want %v.\n"+
			"Code that forgets to populate a set must degrade to cm's previous behavior rather than to a "+
			"confident wrong answer.", got, want)
	}
}

// A peer that reports the mechanism but not a given capability is the conclusive no.
func TestAPeerReportingTheMechanismWithoutACapabilitySaysAbsent(t *testing.T) {
	got := supportOfEach(Parse([]string{string(Reported)}))

	// Everything Absent except the one token that was reported, which is the shape that separates a
	// conclusive no from silence.
	want := everyDeclared(Absent)
	want[Reported] = Present
	if !reflect.DeepEqual(got, want) {
		t.Errorf("a peer reporting only %q answers %v, want %v", Reported, got, want)
	}
}

// TestThisBuildsShimSetSurvivesTheWire covers the round trip a StateResponse makes.
//
// Strings goes out and Parse comes back, so a set that did not survive the pair would make a peer look
// like it lacked something it had. Asserted over the whole set rather than a token at a time.
func TestThisBuildsShimSetSurvivesTheWire(t *testing.T) {
	shim := Shim()

	got := Parse(shim.Strings())

	if !slices.Equal(got.Names(), shim.Names()) {
		t.Errorf("Parse(Shim().Strings()).Names() = %v, want %v", got.Names(), shim.Names())
	}
	// Every role must declare Reported, or an empty list stops being conclusive and Supports can only
	// ever answer Unknown.
	if !shim.Reports() {
		t.Errorf("Shim() does not declare %q, so a peer running this build is indistinguishable from one "+
			"that predates capability reporting", Reported)
	}
}

// TestATokenThisBuildDoesNotKnowIsKeptAsEvidenceOfANewerPeer covers the direction of skew.
//
// A version string difference says two builds differ without saying which is ahead. A token this build
// has never heard of does say: the peer is newer, so the reader's move is to rebuild this binary rather
// than to restart that peer. Dropping unrecognized tokens at Parse would throw that away.
func TestATokenThisBuildDoesNotKnowIsKeptAsEvidenceOfANewerPeer(t *testing.T) {
	// Tokens no role declares, so they stand in for a capability added after this build. Deliberately not
	// real ones: a token that later becomes declared would silently stop testing anything, which is how a
	// test that cannot fail gets written.
	newer := Parse([]string{string(Reported), string(ShutdownSignal), "attach.teleport", "pty.warp"})

	got := newer.Unrecognized()

	want := []Name{"attach.teleport", "pty.warp"}
	if !slices.Equal(got, want) {
		t.Errorf("Unrecognized() = %v, want %v", got, want)
	}
	// The recognized ones still read normally, so an unknown token is extra information rather than
	// something that disturbs the answers a caller depends on.
	if got := newer.Supports(ShutdownSignal); got != Present {
		t.Errorf("Supports(%q) = %v alongside unknown tokens, want %v", ShutdownSignal, got, Present)
	}
}

// Missing counts Unknown as missing, since a diagnostic reports what cannot be relied on.
func TestMissingCountsWhatCannotBeConfirmed(t *testing.T) {
	tests := []struct {
		name string
		set  Set
		want []Name
	}{
		{"a peer that reported nothing", Parse(nil), []Name{Reported, ShutdownSignal}},
		{"a peer missing one", Parse([]string{string(Reported)}), []Name{ShutdownSignal}},
		{"this build's shim", Shim(), nil},
		{"a peer holding a superset", Parse(append(Shim().Strings(), "pty.warp")), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.set.Missing(Reported, ShutdownSignal)
			if !slices.Equal(got, tt.want) {
				t.Errorf("Missing() = %v, want %v", got, tt.want)
			}
		})
	}
}

// everyDeclared builds the expectation that every capability answers the same way.
//
// Derived from Declared() rather than written out, so adding a token does not silently leave a test
// asserting a stale subset. Written out, the three tests using this passed for two tokens and stopped
// covering the two added after them.
func everyDeclared(s Support) map[Name]Support {
	out := make(map[Name]Support)
	for _, n := range Declared() {
		out[n] = s
	}
	return out
}

// supportOfEach answers for every declared capability at once, so a test asserts the whole set rather
// than the one token it happens to be thinking about.
func supportOfEach(s Set) map[Name]Support {
	out := make(map[Name]Support)
	for _, n := range Declared() {
		out[n] = s.Supports(n)
	}
	return out
}

// TestEveryDeclaredCapabilityIsUsedSomewhere is the discipline this mechanism needs to stay honest.
//
// A self-reported capability list is only as good as the habit behind it, and the failure mode is a
// registry that accumulates tokens nothing consults: a peer then advertises something no caller checks,
// which is all of the bookkeeping and none of the benefit. Worse, it reads as coverage. The rule is that
// a token exists because some call site asks about it, and this is that rule as a test rather than as a
// paragraph in a doc.
//
// Scoped to uses outside this package, since declaring a token in a role set is not consulting it.
func TestEveryDeclaredCapabilityIsUsedSomewhere(t *testing.T) {
	root := moduleRoot(t)
	tokens := declaredIdentifiers(t)

	// Reported is the exception, and it is one by construction rather than by oversight. It is not a
	// feature any caller gates on: it is how Supports tells an empty list from a conclusive no, so its
	// only consumer is Reports in this package.
	delete(tokens, "Reported")

	used := make(map[string]bool)
	fset := token.NewFileSet()
	files := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// third_party holds the vendored libghostty checkout, and the two dot directories hold
			// worktrees and tooling. None of them gate on a cm capability.
			switch d.Name() {
			case "third_party", ".git", ".worktrees":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// This package's own files are skipped: a role set naming a token is the declaration, not a use.
		if filepath.Dir(path) == filepath.Join(root, "internal", "capability") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return fmt.Errorf("parsing %s: %w", path, perr)
		}
		files++
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "capability" {
				return true
			}
			used[sel.Sel.Name] = true
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files == 0 {
		t.Fatal("no source files were scanned, so this test proves nothing")
	}

	for ident, tok := range tokens {
		if !used[ident] {
			t.Errorf("capability %q (capability.%s) is declared and nothing outside this package asks "+
				"about it.\nA token exists because a call site gates on it. Either add the gate that "+
				"motivated it, or drop the token: a peer advertising a capability no caller checks is the "+
				"bookkeeping without the benefit, and it reads as coverage.", tok, ident)
		}
	}
}

// declaredIdentifiers maps each capability constant's Go name to its token, read out of this package's
// own source.
//
// Parsed rather than listed, because a list here would be a second place to forget.
func declaredIdentifiers(t *testing.T) map[string]string {
	t.Helper()

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "capability.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	out := make(map[string]string)
	for _, d := range f.Decls {
		gen, ok := d.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			// Only the Name-typed constants. Support's constants are in an untyped iota block and are not
			// capabilities.
			id, ok := vs.Type.(*ast.Ident)
			if !ok || id.Name != "Name" {
				continue
			}
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				tok, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquoting %s: %v", name.Name, err)
				}
				out[name.Name] = tok
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("no capability constants were found, so this test proves nothing")
	}
	// A token declared but left out of every role set would be advertised by nobody, which is the same
	// dead weight from the other direction.
	for ident, tok := range out {
		if _, ok := declared[Name(tok)]; !ok {
			t.Errorf("capability %q (capability.%s) is declared and belongs to no role's set, so no peer "+
				"ever reports it", tok, ident)
		}
	}
	return out
}

// moduleRoot walks up to the directory holding go.mod, so the scan covers the whole repo from wherever
// the test binary runs.
func moduleRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the working directory, so the repo cannot be scanned")
		}
		dir = parent
	}
}
