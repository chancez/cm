package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

func TestResolveGrepCommand(t *testing.T) {
	found := func(names ...string) func(string) (string, error) {
		return func(name string) (string, error) {
			for _, n := range names {
				if n == name {
					return "/usr/bin/" + name, nil
				}
			}
			return "", errors.New("not found")
		}
	}
	type result struct {
		command string
		err     bool
	}
	for _, tc := range []struct {
		name       string
		flag, conf string
		path       func(string) (string, error)
		want       result
	}{
		{"flag wins", "ugrep -i", "grep", found("rg", "grep"), result{command: "ugrep -i"}},
		{"config next", "", "rg -S", found("rg", "grep"), result{command: "rg -S"}},
		{"rg when installed", "", "", found("rg", "grep"), result{command: "rg -n"}},
		{"grep without rg", "", "", found("grep"), result{command: "grep -n"}},
		{"neither", "", "", found(), result{err: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command, err := resolveGrepCommand(tc.flag, tc.conf, tc.path)
			if got := (result{command: command, err: err != nil}); got != tc.want {
				t.Errorf("resolveGrepCommand() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// The pattern reaches grep as one argument whatever it holds: a leading dash, quotes, and a command
// substitution that would create a file if the shell ever parsed it.
func TestRunGrepPassesThePatternVerbatim(t *testing.T) {
	dir := t.TempDir()
	pattern := `-x'"$(touch pwned)`
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("nothing\n"+pattern+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b"), []byte("other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	type result struct {
		code   int
		stdout string
	}

	var out bytes.Buffer
	code, err := runGrep(context.Background(), "grep -nF", pattern, dir, []string{"a", "b"}, &out, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := (result{code, out.String()}), (result{0, "a:2:" + pattern + "\n"}); got != want {
		t.Errorf("runGrep() = %+v, want %+v", got, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "pwned")); err == nil {
		t.Error("the pattern was run as a command")
	}

	out.Reset()
	code, err = runGrep(context.Background(), "grep -nF", "absent", dir, []string{"a", "b"}, &out, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := (result{code, out.String()}), (result{1, ""}); got != want {
		t.Errorf("runGrep() with no match = %+v, want %+v", got, want)
	}
}

func TestGrepTargetsNameFilesBySpaceAndName(t *testing.T) {
	got := grepTargets([]*serverv1.Session{
		{Id: "a1", Name: "kitty.12", Space: "work"},
		{Id: "b2", Space: "work"},
		{Id: "c3", Name: "loose"},
	})
	want := []grepTarget{
		{ref: "@c3", file: "loose"},
		{ref: "@b2", file: filepath.Join("work", "@b2")},
		{ref: "@a1", file: filepath.Join("work", "kitty.12")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("grepTargets() = %+v, want %+v", got, want)
	}
}

// pagedHistory serves walks in order, one slice of pages per walk; a nil page stands for discarded.
type pagedHistory struct {
	walks    [][][]byte
	walk     int
	page     int
	wholeLog []byte
}

func (p *pagedHistory) History(context.Context, *serverv1.HistoryRequest) (*serverv1.HistoryResponse, error) {
	return &serverv1.HistoryResponse{Data: p.wholeLog}, nil
}

func (p *pagedHistory) HistoryPage(
	_ context.Context, req *serverv1.HistoryPageRequest,
) (*serverv1.HistoryPageResponse, error) {
	if req.Cursor == "" {
		if p.page != 0 {
			p.walk++
		}
		p.page = 0
	}
	pages := p.walks[p.walk]
	data := pages[p.page]
	p.page++
	if data == nil {
		p.page = 0
		p.walk++
		return &serverv1.HistoryPageResponse{Discarded: true}, nil
	}
	if p.page == len(pages) {
		return &serverv1.HistoryPageResponse{Data: data, Done: true}, nil
	}
	return &serverv1.HistoryPageResponse{Data: data, Cursor: "c"}, nil
}

// A walk discarded part way is started again once, and the file holds only the walk that finished.
func TestWriteHistoryFile(t *testing.T) {
	type result struct {
		contents string
		err      bool
	}
	for _, tc := range []struct {
		name  string
		paged bool
		cl    *pagedHistory
		want  result
	}{
		{"pages", true, &pagedHistory{walks: [][][]byte{{[]byte("a\n"), []byte("b\n")}}},
			result{contents: "a\nb\n"}},
		{"discarded then read", true, &pagedHistory{walks: [][][]byte{
			{[]byte("old\n"), nil},
			{[]byte("new\n"), []byte("end\n")},
		}}, result{contents: "new\nend\n"}},
		{"discarded twice", true, &pagedHistory{walks: [][][]byte{
			{[]byte("x\n"), nil},
			{[]byte("y\n"), nil},
		}}, result{contents: "", err: true}},
		{"older server", false, &pagedHistory{wholeLog: []byte("whole\n")}, result{contents: "whole\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			target := grepTarget{ref: "@a1", file: filepath.Join("work", "w")}
			err := writeHistoryFile(context.Background(), tc.cl, tc.paged, target, dir)
			contents, _ := os.ReadFile(filepath.Join(dir, "work", "w"))
			if got := (result{contents: string(contents), err: err != nil}); got != tc.want {
				t.Errorf("writeHistoryFile() = %+v (err %v), want %+v", got, err, tc.want)
			}
		})
	}
}
