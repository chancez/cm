package main

import (
	"slices"
	"testing"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

func TestSessionCandidates(t *testing.T) {
	sessions := []*serverv1.Session{
		{Id: "a7k2m9x4", Names: []string{"work"}},
		{Id: "a7zzzzzz", Names: []string{"web", "www"}, Clients: 1},
		{Id: "b3c4d5e6"},
	}
	tests := []struct {
		prefix string
		want   []string
	}{
		{
			prefix: "",
			want:   []string{"work\trunning", "web\trunning, attached", "www\trunning, attached", "@b3c4d5e6\trunning"},
		},
		{
			// The sigil asks for IDs, so a named session offers its ID too.
			prefix: "@",
			want:   []string{"@a7k2m9x4\trunning", "@a7zzzzzz\trunning, attached", "@b3c4d5e6\trunning"},
		},
		{
			prefix: "@a7k",
			want:   []string{"@a7k2m9x4\trunning"},
		},
		{
			prefix: "@b",
			want:   []string{"@b3c4d5e6\trunning"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.prefix, func(t *testing.T) {
			got := sessionCandidates(sessions, tt.prefix, nil)
			if !slices.Equal(got, tt.want) {
				t.Errorf("sessionCandidates(%q) = %q, want %q", tt.prefix, got, tt.want)
			}
		})
	}
}
