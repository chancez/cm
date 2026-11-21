package main

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// fakeRenamer answers List from a canned set and records the binds and unbinds it was asked for.
type fakeRenamer struct {
	sessions  []*serverv1.Session
	unbindErr error

	bound   []*serverv1.BindRequest
	unbound []*serverv1.UnbindRequest
}

func (f *fakeRenamer) List(context.Context, *serverv1.ListRequest) (*serverv1.ListResponse, error) {
	return &serverv1.ListResponse{Sessions: f.sessions}, nil
}

func (f *fakeRenamer) Bind(_ context.Context, req *serverv1.BindRequest) (*serverv1.BindResponse, error) {
	f.bound = append(f.bound, req)
	return &serverv1.BindResponse{}, nil
}

func (f *fakeRenamer) Unbind(_ context.Context, req *serverv1.UnbindRequest) (*serverv1.UnbindResponse, error) {
	f.unbound = append(f.unbound, req)
	return &serverv1.UnbindResponse{}, f.unbindErr
}

// A rename binds by ID and drops the old name only when it was the only one, the rule cm tui's r uses.
func TestRenameSession(t *testing.T) {
	tests := []struct {
		desc        string
		session     *serverv1.Session
		ref         string
		want        renamed
		wantUnbound []*serverv1.UnbindRequest
	}{
		{
			desc:        "one name is replaced",
			session:     &serverv1.Session{Id: "a7k2m9x4", Name: "work", Names: []string{"work"}},
			ref:         "work",
			want:        renamed{label: "work", name: "refactor", sessionID: "a7k2m9x4", dropped: "work"},
			wantUnbound: []*serverv1.UnbindRequest{{Name: "work"}},
		},
		{
			desc:    "several names are all kept",
			session: &serverv1.Session{Id: "a7k2m9x4", Name: "work", Names: []string{"work", "kitty.325"}},
			ref:     "kitty.325",
			want:    renamed{label: "work", name: "refactor", sessionID: "a7k2m9x4"},
		},
		{
			// Name holds the ID reference here, which must not be unbound as though it were a name.
			desc:    "a session with no name gains one",
			session: &serverv1.Session{Id: "a7k2m9x4", Name: "@a7k2m9x4"},
			ref:     "@a7k2m9x4",
			want:    renamed{label: "@a7k2m9x4", name: "refactor", sessionID: "a7k2m9x4"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			f := &fakeRenamer{sessions: []*serverv1.Session{tt.session}}
			got, err := renameSession(context.Background(), f, "refactor", tt.ref)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("renameSession = %+v, want %+v", got, tt.want)
			}
			wantBound := []*serverv1.BindRequest{{Name: "refactor", Session: "@a7k2m9x4"}}
			if !reflect.DeepEqual(f.bound, wantBound) {
				t.Errorf("bound %v, want %v", f.bound, wantBound)
			}
			if !reflect.DeepEqual(f.unbound, tt.wantUnbound) {
				t.Errorf("unbound %v, want %v", f.unbound, tt.wantUnbound)
			}
		})
	}
}

// An unbind that fails leaves two names, and the error says so rather than reporting a failed rename.
func TestRenameSessionReportsAnOldNameItCouldNotDrop(t *testing.T) {
	f := &fakeRenamer{
		sessions:  []*serverv1.Session{{Id: "a7k2m9x4", Name: "work", Names: []string{"work"}}},
		unbindErr: errors.New("server went away"),
	}
	_, err := renameSession(context.Background(), f, "refactor", "work")
	if got, want := err.Error(), "work is now refactor, but work still names it too: server went away"; got != want {
		t.Errorf("error %q, want %q", got, want)
	}
}

func TestRenamedWrite(t *testing.T) {
	var buf bytes.Buffer
	r := renamed{label: "work", name: "refactor", sessionID: "a7k2m9x4", dropped: "work"}
	if err := r.write(&buf, false); err != nil {
		t.Fatal(err)
	}
	if got, want := buf.String(), "work is now refactor, and no longer work\n"; got != want {
		t.Errorf("text %q, want %q", got, want)
	}
}
