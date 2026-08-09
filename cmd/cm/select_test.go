package main

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// selectClient answers List from a fixed set of sessions, so the selector rules can be tested without
// a server.
//
// Only List is implemented: sessionTargets exists to turn a selector into names, and the commands that
// then act on those names are covered end to end. Everything else panics rather than returning a zero
// value, so a test that reaches further fails loudly instead of silently passing.
type selectClient struct {
	serverv1.ServerClient
	sessions []*serverv1.Session
	// gotTags records the selector the client was asked for, so a test can check it was forwarded
	// rather than reimplemented here.
	gotTags []string
}

func (c *selectClient) List(
	_ context.Context, req *serverv1.ListRequest,
) (*serverv1.ListResponse, error) {
	c.gotTags = req.Tags
	// Filtered here the way the server does, since the point of these tests is the client's handling of
	// the result rather than the matching itself, which internal/tags covers.
	var out []*serverv1.Session
	for _, s := range c.sessions {
		if matchesAll(s.Tags, req.Tags) {
			out = append(out, s)
		}
	}
	return &serverv1.ListResponse{Sessions: out}, nil
}

// matchesAll reports whether a session's tags satisfy every selector term.
func matchesAll(tags map[string]string, selectors []string) bool {
	for _, sel := range selectors {
		key, value, hasValue := strings.Cut(sel, "=")
		v, ok := tags[key]
		if !ok {
			return false
		}
		if hasValue && value != "" && v != value {
			return false
		}
	}
	return true
}

func newSelectClient() *selectClient {
	mk := func(name string, tags map[string]string, created int64) *serverv1.Session {
		return &serverv1.Session{Name: name, Tags: tags, CreatedAtUnix: created}
	}
	return &selectClient{sessions: []*serverv1.Session{
		mk("one", map[string]string{"run": "abc", "area": "api"}, 100),
		mk("two", map[string]string{"run": "abc", "area": "ui"}, 200),
		mk("three", map[string]string{"run": "xyz"}, 300),
		mk("plain", nil, 400),
	}}
}

func TestSessionTargets(t *testing.T) {
	tests := []struct {
		name             string
		args             []string
		selectors        []string
		wantNames        []string
		wantFromSelector bool
	}{
		{
			name:      "a named session",
			args:      []string{"one"},
			wantNames: []string{"one"},
		},
		{
			name:             "a selector matching several",
			selectors:        []string{"run=abc"},
			wantNames:        []string{"one", "two"},
			wantFromSelector: true,
		},
		{
			// fromSelector stays true for a single match, which is what makes the output headed: the
			// caller did not know which session it would be, so the name is part of the answer.
			name:             "a selector matching one",
			selectors:        []string{"area=ui"},
			wantNames:        []string{"two"},
			wantFromSelector: true,
		},
		{
			name:             "two terms narrow",
			selectors:        []string{"run=abc", "area=api"},
			wantNames:        []string{"one"},
			wantFromSelector: true,
		},
		{
			name:             "a bare key",
			selectors:        []string{"area"},
			wantNames:        []string{"one", "two"},
			wantFromSelector: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cl := newSelectClient()
			names, fromSelector, err := sessionTargets(context.Background(), cl, tc.args, tc.selectors)
			if err != nil {
				t.Fatalf("sessionTargets() error = %v", err)
			}
			if !reflect.DeepEqual(names, tc.wantNames) {
				t.Errorf("names = %v, want %v", names, tc.wantNames)
			}
			if fromSelector != tc.wantFromSelector {
				t.Errorf("fromSelector = %v, want %v", fromSelector, tc.wantFromSelector)
			}
		})
	}
}

// A name and a selector together is a mistake rather than an intersection.
//
// There is no reading of `cm read foo --tag bar` that is not a confusion about which one applies, and
// picking either would act on sessions the caller did not ask for.
func TestSessionTargetsRejectsNameAndSelector(t *testing.T) {
	cl := newSelectClient()
	_, _, err := sessionTargets(context.Background(), cl, []string{"one"}, []string{"run=abc"})
	if err == nil {
		t.Fatal("sessionTargets() = nil error for a name and a selector, want one")
	}
	// Names both halves, since the fix is dropping one and the user has to know which they are choosing
	// between.
	for _, want := range []string{"one", "--tag"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

// A selector matching nothing is an error, not an empty success.
//
// The dangerous case this prevents: `cm kill --tag run=typo` exiting 0 having killed nothing looks
// exactly like a successful teardown, and a script would move on believing it cleaned up.
func TestSessionTargetsRejectsAnEmptyMatch(t *testing.T) {
	cl := newSelectClient()
	_, _, err := sessionTargets(context.Background(), cl, nil, []string{"run=absent"})
	if err == nil {
		t.Fatal("sessionTargets() = nil error for a selector matching nothing, want one")
	}
	// Echoes the selector, since the usual cause is a typo in it.
	if !strings.Contains(err.Error(), "run=absent") {
		t.Errorf("error = %q, want it to echo the selector", err)
	}
}

// Neither a name nor a selector is an error rather than a default to everything.
func TestSessionTargetsRequiresATarget(t *testing.T) {
	cl := newSelectClient()
	_, _, err := sessionTargets(context.Background(), cl, nil, nil)
	if !errors.Is(err, errNoSessionGiven) {
		t.Errorf("sessionTargets() error = %v, want errNoSessionGiven", err)
	}
}

// The selector goes to the server rather than being matched client-side.
//
// Matters because the server owns what a tag means: a second implementation here would drift from it,
// and the JSON column it reads is not something the client sees.
func TestSessionTargetsForwardsTheSelector(t *testing.T) {
	cl := newSelectClient()
	if _, _, err := sessionTargets(
		context.Background(), cl, nil, []string{"run=abc", "area=api"},
	); err != nil {
		t.Fatalf("sessionTargets() error = %v", err)
	}
	want := []string{"run=abc", "area=api"}
	if !reflect.DeepEqual(cl.gotTags, want) {
		t.Errorf("List got tags %v, want %v", cl.gotTags, want)
	}
}

// An invalid session name is rejected before a server is contacted.
func TestSessionTargetsValidatesAName(t *testing.T) {
	cl := newSelectClient()
	if _, _, err := sessionTargets(context.Background(), cl, []string{"../escape"}, nil); err == nil {
		t.Error("sessionTargets() = nil error for a name with a separator, want one")
	}
}

// Results come back in list order, so output across sessions is stable between calls.
func TestResolveSelectorIsOrdered(t *testing.T) {
	cl := newSelectClient()
	// Reversed, so an implementation that returned them as received would fail.
	cl.sessions[0], cl.sessions[1] = cl.sessions[1], cl.sessions[0]

	names, err := resolveSelector(context.Background(), cl, []string{"run=abc"})
	if err != nil {
		t.Fatalf("resolveSelector() error = %v", err)
	}
	// Oldest first, which is what sortSessions does, so "one" at 100 precedes "two" at 200.
	if want := []string{"one", "two"}; !reflect.DeepEqual(names, want) {
		t.Errorf("resolveSelector() = %v, want %v", names, want)
	}
}

func TestValidateSelectors(t *testing.T) {
	if err := validateSelectors([]string{"run=abc", "area"}); err != nil {
		t.Errorf("validateSelectors() error = %v, want nil", err)
	}
	// Client-side so a typo is reported without a server, and the same way whether one is running.
	if err := validateSelectors([]string{"bad key"}); err == nil {
		t.Error("validateSelectors() = nil for a key with a space, want an error")
	}
}

func TestWriteSessionHeader(t *testing.T) {
	var buf bytes.Buffer
	if err := writeSessionHeader(&buf, "first", true); err != nil {
		t.Fatalf("writeSessionHeader() error = %v", err)
	}
	if err := writeSessionHeader(&buf, "second", false); err != nil {
		t.Fatalf("writeSessionHeader() error = %v", err)
	}

	// A blank line between sessions but not before the first, so the output does not open with one and
	// the last does not trail one.
	want := "=== first ===\n\n=== second ===\n"
	if got := buf.String(); got != want {
		t.Errorf("headers = %q, want %q", got, want)
	}
}

func TestSessionOrTagArg(t *testing.T) {
	if err := sessionOrTagArg(nil, nil); err != nil {
		t.Errorf("sessionOrTagArg(none) error = %v, want nil: --tag supplies the target", err)
	}
	if err := sessionOrTagArg(nil, []string{"one"}); err != nil {
		t.Errorf("sessionOrTagArg(one) error = %v, want nil", err)
	}
	err := sessionOrTagArg(nil, []string{"one", "two"})
	if err == nil {
		t.Fatal("sessionOrTagArg(two) = nil, want an error")
	}
	// Points at --tag, since someone passing two names is reaching for the thing that takes several.
	if !strings.Contains(err.Error(), "--tag") {
		t.Errorf("error = %q, want it to suggest --tag", err)
	}
}
