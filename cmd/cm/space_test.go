package main

import (
	"reflect"
	"testing"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

func TestSpaceCandidates(t *testing.T) {
	all := []*serverv1.Space{
		{Name: "cm", Sessions: 3, Running: 2},
		{Name: "default", Sessions: 1, Running: 1},
		{Name: "infra"},
	}
	if got, want := spaceCandidates(all, "", nil), []string{
		"cm\t3 sessions, 2 running", "default\t1 sessions, 1 running", "infra\t0 sessions, 0 running",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("all = %q, want %q", got, want)
	}
	if got, want := spaceCandidates(all, "c", nil), []string{"cm\t3 sessions, 2 running"}; !reflect.DeepEqual(got, want) {
		t.Errorf("prefix c = %q, want %q", got, want)
	}
	// remove offers only what it accepts: empty, and not default.
	if got, want := spaceCandidates(all, "", removableSpace), []string{"infra\t0 sessions, 0 running"}; !reflect.DeepEqual(got, want) {
		t.Errorf("removable = %q, want %q", got, want)
	}
}
