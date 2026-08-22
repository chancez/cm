package server

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/chancez/cm/internal/store"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// tagTestSession records a session with the given tags, without spawning a shim.
//
// Tags live in the store rather than the registry, so nothing here needs a live session. That is the
// point of testing at this seam: the behavior under test is bookkeeping, and a pty would only add a
// way for the test to be flaky.
func tagTestSession(t *testing.T, st *store.Store, name string, sessionTags map[string]string) {
	t.Helper()
	if err := st.Create(context.Background(), store.Session{
		ID:    name,
		State: store.StateRunning,
		Tags:  sessionTags,
	}); err != nil {
		t.Fatalf("Create(%s) error = %v", name, err)
	}
	// Named as well as recorded: these tests reach their session through the resolve layer, and a
	// listing shows a session with no names by its ID.
	nameSession(t, st, name)
}

func TestSetTagsMergesByDefault(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	ctx := context.Background()
	tagTestSession(t, st, "work", map[string]string{"project": "cm", "role": "reviewer"})

	got, err := mgr.SetTags(ctx, "work", map[string]string{"role": "builder", "extra": "1"}, nil, false)
	if err != nil {
		t.Fatalf("SetTags() error = %v", err)
	}
	// project survives untouched, role is overwritten, extra is added.
	want := map[string]string{"project": "cm", "role": "builder", "extra": "1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SetTags() = %v, want %v", got, want)
	}

	// And the store agrees, since the returned value would otherwise be reporting an intention
	// rather than a result.
	rec, err := st.Get(ctx, "work")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !reflect.DeepEqual(rec.Tags, want) {
		t.Errorf("stored tags = %v, want %v", rec.Tags, want)
	}
}

func TestSetTagsRemove(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	ctx := context.Background()
	tagTestSession(t, st, "work", map[string]string{"project": "cm", "role": "reviewer"})

	got, err := mgr.SetTags(ctx, "work", nil, []string{"role"}, false)
	if err != nil {
		t.Fatalf("SetTags() error = %v", err)
	}
	want := map[string]string{"project": "cm"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SetTags() = %v, want %v", got, want)
	}
}

// Removing a key that was never there is not an error, since the caller's intent is satisfied.
func TestSetTagsRemoveMissingKey(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	tagTestSession(t, st, "work", map[string]string{"project": "cm"})

	got, err := mgr.SetTags(context.Background(), "work", nil, []string{"absent"}, false)
	if err != nil {
		t.Fatalf("SetTags() error = %v", err)
	}
	want := map[string]string{"project": "cm"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SetTags() = %v, want %v", got, want)
	}
}

// Removing the last tag must leave none, not leave the previous set in place.
//
// This is what the store's nil-versus-empty distinction exists for: a map that came back nil here
// would tell the store to leave the column alone, so the tags would survive their own removal.
func TestSetTagsRemovingTheLastTagClearsThem(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	ctx := context.Background()
	tagTestSession(t, st, "work", map[string]string{"project": "cm"})

	got, err := mgr.SetTags(ctx, "work", nil, []string{"project"}, false)
	if err != nil {
		t.Fatalf("SetTags() error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("SetTags() = %v, want no tags", got)
	}

	rec, err := st.Get(ctx, "work")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if len(rec.Tags) != 0 {
		t.Errorf("stored tags = %v, want none: removing the last tag must persist", rec.Tags)
	}
}

func TestSetTagsReplaceDiscardsTheRest(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	ctx := context.Background()
	tagTestSession(t, st, "work", map[string]string{"project": "cm", "role": "reviewer"})

	got, err := mgr.SetTags(ctx, "work", map[string]string{"fresh": "1"}, nil, true)
	if err != nil {
		t.Fatalf("SetTags() error = %v", err)
	}
	want := map[string]string{"fresh": "1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SetTags() = %v, want only the tags given", got)
	}
}

// Removing and setting the same key in one call removes it, since remove is the more specific
// instruction. Either order is defensible, so the chosen one is pinned rather than left to drift.
func TestSetTagsRemoveWinsOverSet(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	ctx := context.Background()
	tagTestSession(t, st, "work", nil)

	got, err := mgr.SetTags(ctx, "work", map[string]string{"k": "v"}, []string{"k"}, false)
	if err != nil {
		t.Fatalf("SetTags() error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("SetTags() = %v, want the removal to win", got)
	}
}

// A session that does not exist cannot be tagged, and the error has to say which.
func TestSetTagsUnknownSession(t *testing.T) {
	mgr, _, _ := newTestManager(t, nil)
	_, err := mgr.SetTags(context.Background(), "ghost", map[string]string{"k": "v"}, nil, false)
	if err == nil {
		t.Fatal("SetTags() = nil error for an unknown session, want one")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error = %q, want it to name the session", err)
	}
}

// The RPC validates tags, because the socket is the trust boundary rather than the CLI.
//
// A tag reaching the store unchecked would be printed to the terminal of whoever runs `cm list`, so a
// client that skips the CLI's parsing must still be refused. This is the check that makes the
// character set a guarantee rather than a convention.
func TestTagRPCRejectsHostileTags(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	svc := &Service{mgr: mgr}
	ctx := context.Background()
	tagTestSession(t, st, "work", nil)

	hostile := []*serverv1.TagRequest{
		// An escape sequence that would retitle the listing terminal.
		{Session: "work", Set: map[string]string{"k": "\x1b]2;pwned\x07"}},
		{Session: "work", Set: map[string]string{"\x1b]2;pwned\x07": "v"}},
		// A newline, which would break the table into a forged row.
		{Session: "work", Set: map[string]string{"k": "a\nb"}},
		// Over the length limit.
		{Session: "work", Set: map[string]string{"k": strings.Repeat("a", 64)}},
		{Session: "work", Set: map[string]string{strings.Repeat("a", 64): "v"}},
		// A removal names a key too, so it gets the same check.
		{Session: "work", Remove: []string{"a b"}},
	}

	for _, req := range hostile {
		if _, err := svc.Tag(ctx, req); err == nil {
			t.Errorf("Tag(%+v) = nil error, want it refused", req)
		}
	}

	// And nothing was written by any of the refused calls.
	rec, err := st.Get(ctx, "work")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if len(rec.Tags) != 0 {
		t.Errorf("stored tags = %v, want none after every call was refused", rec.Tags)
	}
}

// A call that changes nothing is an error rather than a silent success, so a caller that meant to
// change something learns it did not.
func TestTagRPCRequiresSomethingToDo(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	svc := &Service{mgr: mgr}
	tagTestSession(t, st, "work", nil)

	if _, err := svc.Tag(context.Background(), &serverv1.TagRequest{Session: "work"}); err == nil {
		t.Error("Tag() with no set or remove = nil error, want one")
	}
}

func TestTagRPCRequiresASession(t *testing.T) {
	mgr, _, _ := newTestManager(t, nil)
	svc := &Service{mgr: mgr}

	_, err := svc.Tag(context.Background(), &serverv1.TagRequest{
		Set: map[string]string{"k": "v"},
	})
	if err == nil {
		t.Error("Tag() with no session = nil error, want one")
	}
}

// An exited session can still be tagged, unlike a report.
//
// The difference is deliberate: a report describes a running program and is held in memory, while a
// tag describes the session. Labelling a finished run to keep track of it is reasonable, and refusing
// would be a rule with nothing behind it.
func TestTagWorksOnAnExitedSession(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	svc := &Service{mgr: mgr}
	ctx := context.Background()

	if err := st.Create(ctx, store.Session{
		ID:    "finished",
		State: store.StateExited,
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	// An exited session keeps its names, which is what makes tagging one by name possible at all.
	nameSession(t, st, "finished")

	resp, err := svc.Tag(ctx, &serverv1.TagRequest{
		Session: "finished",
		Set:     map[string]string{"outcome": "failed"},
	})
	if err != nil {
		t.Fatalf("Tag() on an exited session error = %v", err)
	}
	want := map[string]string{"outcome": "failed"}
	if !reflect.DeepEqual(resp.Tags, want) {
		t.Errorf("Tag() = %v, want %v", resp.Tags, want)
	}
}

// List filters by tag, with repeated terms narrowing rather than widening.
func TestListFiltersByTag(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	svc := &Service{mgr: mgr}
	ctx := context.Background()

	tagTestSession(t, st, "a", map[string]string{"project": "cm", "role": "reviewer"})
	tagTestSession(t, st, "b", map[string]string{"project": "cm", "role": "builder"})
	tagTestSession(t, st, "c", map[string]string{"project": "zmx"})
	tagTestSession(t, st, "d", nil)

	tests := []struct {
		name string
		tags []string
		want []string
	}{
		{name: "no filter", tags: nil, want: []string{"a", "b", "c", "d"}},
		{name: "by value", tags: []string{"project=cm"}, want: []string{"a", "b"}},
		{name: "bare key", tags: []string{"role"}, want: []string{"a", "b"}},
		// Two terms means both, which is the whole reason repeating narrows.
		{name: "two terms", tags: []string{"project=cm", "role=builder"}, want: []string{"b"}},
		{name: "no matches", tags: []string{"project=absent"}, want: nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := svc.List(ctx, &serverv1.ListRequest{Tags: tc.tags})
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			var got []string
			for _, s := range resp.Sessions {
				got = append(got, s.Name)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("List(tags=%v) = %v, want %v", tc.tags, got, tc.want)
			}
		})
	}
}

// A malformed selector is an error rather than a silent match of everything.
//
// It matters because the wrong answer here is dangerous rather than merely wrong: a `cm kill --tag`
// built on a selector that quietly matched everything would kill every session.
func TestListRejectsAMalformedSelector(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	svc := &Service{mgr: mgr}
	tagTestSession(t, st, "a", nil)

	_, err := svc.List(context.Background(), &serverv1.ListRequest{Tags: []string{"bad key"}})
	if err == nil {
		t.Error("List() with a malformed selector = nil error, want one")
	}
}

// A recreated session keeps its tags.
//
// Open deletes the old record and creates a new one, so anything not carried across here is lost.
func TestInheritForRestoreCarriesTags(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)
	mgr.SetPersistPolicy(testPolicy())

	rec := store.Session{
		ID:      "revived",
		LogPath: dirs.SessionLog("revived"),
		Tags:    map[string]string{"project": "cm", "role": "reviewer"},
	}

	got := mgr.inheritForRestore(OpenOptions{Ref: "revived"}, rec)
	want := map[string]string{"project": "cm", "role": "reviewer"}
	if !reflect.DeepEqual(got.Tags, want) {
		t.Errorf("Tags = %v, want the recorded tags %v", got.Tags, want)
	}
}

// Tags survive even with persistence off and no saved log.
//
// The gap this closes: the record is deleted whether or not it had a log, so carrying tags only on the
// persistence path would silently drop them on a plain install, and on any session whose shell exited
// and was attached to again. Tags describe the session rather than its content, so they do not depend
// on content being kept.
func TestInheritForRestoreCarriesTagsWithoutPersistence(t *testing.T) {
	mgr, _, _ := newTestManager(t, nil)
	// No persist policy at all, which is what an install with no config gets.

	rec := store.Session{
		ID:   "revived",
		Tags: map[string]string{"project": "cm"},
	}

	got := mgr.inheritForRestore(OpenOptions{Ref: "revived"}, rec)
	want := map[string]string{"project": "cm"}
	if !reflect.DeepEqual(got.Tags, want) {
		t.Errorf("Tags = %v, want %v even with persistence off", got.Tags, want)
	}
	// And nothing else was inherited, since there was no log to replay.
	if got.restoreFrom != "" {
		t.Errorf("restoreFrom = %q, want empty with no saved log", got.restoreFrom)
	}
}

// Recorded tags merge with the caller's, and the caller wins per key.
//
// Replacing wholesale would mean that asking for one tag on reattach discarded every other tag, which
// is not what naming one tag says.
func TestInheritForRestoreMergesTags(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)
	mgr.SetPersistPolicy(testPolicy())

	rec := store.Session{
		ID:      "revived",
		LogPath: dirs.SessionLog("revived"),
		Tags:    map[string]string{"project": "cm", "role": "reviewer"},
	}

	got := mgr.inheritForRestore(OpenOptions{
		Ref:  "revived",
		Tags: map[string]string{"role": "builder", "fresh": "1"},
	}, rec)

	want := map[string]string{"project": "cm", "role": "builder", "fresh": "1"}
	if !reflect.DeepEqual(got.Tags, want) {
		t.Errorf("Tags = %v, want %v", got.Tags, want)
	}
}

// List reports a session's tags, since a caller filtering on them also wants to see them.
func TestListReportsTags(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	svc := &Service{mgr: mgr}
	ctx := context.Background()
	tagTestSession(t, st, "work", map[string]string{"project": "cm"})

	resp, err := svc.List(ctx, &serverv1.ListRequest{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(resp.Sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(resp.Sessions))
	}
	want := map[string]string{"project": "cm"}
	if !reflect.DeepEqual(resp.Sessions[0].Tags, want) {
		t.Errorf("Session.Tags = %v, want %v", resp.Sessions[0].Tags, want)
	}
}
