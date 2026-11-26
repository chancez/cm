package main

import (
	"reflect"
	"testing"
)

// cm info names the host it reached a session on, by its name in [remote.hosts], and nothing for this
// machine or for a remote given by address, which is no name a caller can reach it by.
func TestInfoHost(t *testing.T) {
	got := []string{infoHost("white"), infoHost(""), infoHost("ssh://me@white/usr/bin/cm")}
	want := []string{"white", "", ""}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("infoHost = %q, want %q", got, want)
	}
}
