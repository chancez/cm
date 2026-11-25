package vt

import (
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// Compact compresses scrollback once output has been quiet for CompactIdle, waits again after output, and
// compresses again after a read decompressed it without any output at all, which is what the restore every
// attach performs does.
func TestCompactFollowsQuietAndReads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st, err := NewSessionTerminal(50, 200, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()

		write := func(n int) {
			pad := strings.Repeat("x", 100)
			for i := range n {
				if err := st.Write(fmt.Appendf(nil, "line %06d %s\r\n", i, pad)); err != nil {
					t.Fatal(err)
				}
			}
		}
		// drain runs Compact the way the server does, then reports whether most pages ended up compressed.
		drain := func() bool {
			for {
				more, err := st.Compact()
				if err != nil {
					t.Fatal(err)
				}
				if !more {
					break
				}
			}
			state, err := st.term.compressionState()
			if err != nil {
				t.Fatal(err)
			}
			return state.compressedPages*2 > state.pages
		}

		var got []string
		step := func(what string) { got = append(got, fmt.Sprintf("%s: %v", what, drain())) }

		write(20000)
		step("just written")
		step("still within the idle delay")
		time.Sleep(CompactIdle)
		step("quiet for the idle delay")
		if _, err := st.Restore(); err != nil {
			t.Fatal(err)
		}
		// Both before draining, since Restore leaves activity alone and a quiet model compresses at once.
		restored, err := st.term.compressionState()
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("restore decompressed: %v", restored.compressedPages == 0))
		step("after a restore")
		write(1)
		if _, err := st.Restore(); err != nil {
			t.Fatal(err)
		}
		step("restored after new output")
		time.Sleep(CompactIdle)
		step("quiet again")

		want := []string{
			"just written: false",
			"still within the idle delay: false",
			"quiet for the idle delay: true",
			"restore decompressed: true",
			"after a restore: true",
			"restored after new output: false",
			"quiet again: true",
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("compaction:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	})
}
