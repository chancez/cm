package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// localDialers are the functions that reach *this machine's* server, whatever --remote says.
//
// Every one of them is correct in the file it lives in and wrong everywhere else, which is why this is a
// list of names rather than a rule about sockets.
var localDialers = map[string]string{
	"connectServer": "starts and dials the local server",
	"dialServer":    "dials the local server without starting one",
}

// localDialCallers are the files allowed to call one, with the reason each is allowed.
//
// Kept as a map so a failure can print the reason a file is or is not on it, and so adding a file is a
// deliberate act with an argument attached rather than a silent edit.
var localDialCallers = map[string]string{
	// Where the local dialers are defined, and where globals.connect and globals.dial choose between local
	// and remote. This is the seam every other command goes through.
	"server.go": "defines them, and routes by whether a remote was named",
	// Machine-local by classification: it reads this machine's runtime directory, shim logs and processes,
	// so a remote answer would describe the wrong machine's files. See machineLocal.
	"doctor.go": "classified machine-local, so its server is this machine's by definition",
	// The other half of the same seam: serverFor and completionServer each choose the remote when there is
	// one and fall through to the local dial when there is not, which is the choice this test is about.
	"remote.go": "chooses between remote and local, and the local dial is that choice's other branch",
	// `cm upgrade` replaces the binary on the machine it runs on, so it is machine-local by classification
	// and the server whose version it reports before and after is necessarily this one.
	"upgradecm.go": "classified machine-local, since it replaces this machine's binary",
}

// No command reaches the local server directly, because a command that does answers about the wrong
// machine under --remote.
//
// The bug this is the guard for: `cm tui` called connectServer while sitting in remoteCapable, so
// `cm tui --remote ssh://work` drew a picker full of *local* sessions and attaching from it attached
// locally. Nothing failed, no test noticed, and the only clue was that the sessions listed were the wrong
// ones, which is the exact silent-wrong-machine failure the classification lists were built to prevent.
// Reported from real use rather than caught here, which is why the guard exists now.
//
// The classification tests cannot catch this. They check that every command is *named* in one of the three
// lists; nothing in them looks at whether a command named remote-capable actually routes anywhere remote.
// This closes that half: the routing is a call to globals.connect, globals.dial, withServer, serverFor or
// pointAtServer, and a direct local dial outside the two files below is the way to get it wrong.
//
// Parsed rather than grepped so a comment naming connectServer does not trip it, which is how a rule like
// this gets deleted instead of obeyed.
func TestNoCommandDialsTheLocalServerDirectly(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}

	fset := token.NewFileSet()
	checked, found := 0, 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if _, allowed := localDialCallers[name]; allowed {
			continue
		}

		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		checked++
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := call.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			what, local := localDialers[id.Name]
			if !local {
				return true
			}
			found++
			t.Errorf("%s: calls %s, which %s.\n"+
				"A command doing that answers about this machine even when --remote names another one, "+
				"silently. Use withServer or globals.connect, which route by the flag, or add this file to "+
				"localDialCallers with the reason it is exempt.", fset.Position(call.Pos()), id.Name, what)
			return true
		})
	}

	// A scan that inspected nothing would pass forever. Both halves are asserted: that files were read, and
	// that the allow list still names files that exist, since a renamed file would exempt nothing while
	// looking like it did.
	if checked == 0 {
		t.Error("no source files were scanned, so this test proves nothing")
	}
	for name := range localDialCallers {
		if _, err := os.Stat(name); err != nil {
			t.Errorf("localDialCallers names %s, which is not here: %v", name, err)
		}
	}
	if found > 0 {
		t.Logf("%d direct local dials in %d files", found, checked)
	}
}
