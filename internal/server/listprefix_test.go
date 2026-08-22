package server

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/chancez/cm/internal/store"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// listPrefixFixture records four sessions: two named alike, one named differently, one nameless.
func listPrefixFixture(t *testing.T) (*Service, *store.Store) {
	t.Helper()
	mgr, st, _ := newTestManager(t, nil)
	ctx := context.Background()

	for _, id := range []string{"aaaa2222", "bbbb3333", "cccc4444", "dddd5555"} {
		if err := st.Create(ctx, store.Session{ID: id, State: store.StateRunning}); err != nil {
			t.Fatalf("Create(%s) error = %v", id, err)
		}
	}
	for _, b := range []store.Binding{
		{Name: "kitty.1", SessionID: "aaaa2222", OnKill: store.KillTarget},
		{Name: "kitty.2", SessionID: "bbbb3333", OnKill: store.KillTarget},
		{Name: "work", SessionID: "cccc4444", OnKill: store.KillTarget},
		// A second name on one of them, so "matches if any name matches" is exercised.
		{Name: "kitty.9", SessionID: "cccc4444", OnKill: store.KillUnbind},
	} {
		if err := st.Bind(ctx, b); err != nil {
			t.Fatalf("Bind(%s) error = %v", b.Name, err)
		}
	}
	// dddd5555 gets no name at all, which is what `cm attach` with no argument and `cm run -d` leave.
	return NewService(mgr), st
}

// listed returns the ids a List call reports, sorted so the assertion does not depend on ordering.
func listed(t *testing.T, svc *Service, prefix string) []string {
	t.Helper()
	resp, err := svc.List(context.Background(), &serverv1.ListRequest{Prefix: prefix})
	if err != nil {
		t.Fatalf("List(%q) error = %v", prefix, err)
	}
	ids := make([]string, 0, len(resp.Sessions))
	for _, s := range resp.Sessions {
		ids = append(ids, s.Id)
	}
	sort.Strings(ids)
	return ids
}

// A prefix filters on names, and a session matches if any of its names does.
//
// Worth its own test because this moved: it used to be a LIKE in the query, and names now live in their own
// table, so it is matched in Go. That move left it with no coverage at all for a while, which is how a
// filter comes to quietly match everything.
func TestListFiltersByNamePrefix(t *testing.T) {
	svc, _ := listPrefixFixture(t)

	for _, tc := range []struct {
		prefix string
		want   []string
	}{
		// Empty means no filtering, including the session nothing names.
		{"", []string{"aaaa2222", "bbbb3333", "cccc4444", "dddd5555"}},
		{"kitty.", []string{"aaaa2222", "bbbb3333", "cccc4444"}},
		{"kitty.1", []string{"aaaa2222"}},
		// The second name on cccc4444 is what matches here, which is the "any name" rule.
		{"kitty.9", []string{"cccc4444"}},
		{"work", []string{"cccc4444"}},
		{"nothing-like-this", nil},
	} {
		got := listed(t, svc, tc.prefix)
		if len(got) == 0 && len(tc.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("List(prefix=%q) = %v, want %v", tc.prefix, got, tc.want)
		}
	}
}

// A session with no names matches no prefix, which is the honest answer rather than an accident: a prefix is
// a question about names, and a session that has none is not among them. It is still listed unfiltered.
func TestListPrefixSkipsSessionsWithNoNames(t *testing.T) {
	svc, _ := listPrefixFixture(t)

	for _, prefix := range []string{"d", "dddd", "@dddd5555", "dddd5555"} {
		if got := listed(t, svc, prefix); len(got) != 0 {
			t.Errorf("List(prefix=%q) = %v, want nothing: an ID is not a name", prefix, got)
		}
	}
}

// A prefix containing SQL wildcards matches literally.
//
// It cannot do otherwise now that matching is a string comparison in Go, and that is the point of pinning
// it: the previous implementation needed LIKE escaping to avoid `%` matching everything, so anyone moving
// this back into a query has a test that fails rather than a filter that silently matches too much.
func TestListPrefixTreatsWildcardsLiterally(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	svc := NewService(mgr)
	ctx := context.Background()

	if err := st.Create(ctx, store.Session{ID: "aaaa2222", State: store.StateRunning}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := st.Bind(ctx, store.Binding{
		Name: "100_percent", SessionID: "aaaa2222", OnKill: store.KillTarget,
	}); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}

	// Under LIKE these would each match the name above. As literals, none of them does.
	for _, prefix := range []string{"%", "_", "100%", "1_0"} {
		if got := listed(t, svc, prefix); len(got) != 0 {
			t.Errorf("List(prefix=%q) = %v, want nothing: wildcards are literal", prefix, got)
		}
	}
	// And the literal prefix still works.
	if got := listed(t, svc, "100_"); !reflect.DeepEqual(got, []string{"aaaa2222"}) {
		t.Errorf("List(prefix=%q) = %v, want [aaaa2222]", "100_", got)
	}
}
