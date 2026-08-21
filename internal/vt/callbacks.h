// Installs the Go callback trampolines on a terminal.
//
// This is done in C rather than Go for two reasons. The trampolines are Go functions exported
// with //export, and cgo emits their declarations into a generated header that only C compiled
// as part of this package can see. And ghostty_terminal_set takes a *pointer to* the value being
// set, so passing the address of a Go local would hand libghostty a pointer into Go memory,
// which cgo forbids and which crashes once that stack slot is reused.
#ifndef CM_CALLBACKS_H
#define CM_CALLBACKS_H

#include <ghostty/vt.h>

// cm_install_callbacks sets userdata and the callbacks cm uses. Each flag selects whether that
// callback is installed, so a caller that does not want one does not pay for it.
//
// Returns the first non-success result, or GHOSTTY_SUCCESS.
GhosttyResult cm_install_callbacks(GhosttyTerminal terminal,
                                   uintptr_t handle,
                                   bool write_pty,
                                   bool title_changed,
                                   bool pwd_changed,
                                   bool xtversion);

// cm_set_xtversion records the string reported for XTVERSION (CSI > q).
//
// Held in C rather than passed per call because libghostty requires the returned memory to stay
// valid until the callback returns, and a Go string's bytes cannot be handed to C. The value is
// copied into a fixed buffer here, so it outlives any Go allocation and needs no pinning.
//
// Process-wide, not per terminal, which is a real constraint rather than an implementation detail:
// every terminal reports whatever was set last. That is why the Go side exposes this as a package
// function rather than a per-terminal option, so the API cannot promise something it does not do.
// Verified: two terminals constructed with different values both answered the second one.
void cm_set_xtversion(const char *s);

// Modes are built by a static inline function that cgo cannot call, so the ones cm needs are
// exposed as ordinary functions.
GhosttyMode cm_mode_sync_output(void);
GhosttyMode cm_mode_focus_event(void);
GhosttyMode cm_mode_alt_screen_save(void);
GhosttyMode cm_mode_in_band_resize(void);

#endif
