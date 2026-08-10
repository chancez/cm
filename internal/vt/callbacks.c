#include "callbacks.h"
#include "_cgo_export.h"

#include <stdint.h>

// The XTVERSION string, owned here so it outlives the callback that returns it.
//
// libghostty requires the returned memory to stay valid until the callback returns, and a Go
// string's bytes cannot cross into C, so this is a plain C buffer written once at startup. Sized
// well past any version string cm produces; a longer one is truncated rather than overflowing.
static char cm_xtversion[128];

void cm_set_xtversion(const char *s) {
  size_t i = 0;
  for (; s[i] != '\0' && i < sizeof(cm_xtversion) - 1; i++) {
    cm_xtversion[i] = s[i];
  }
  cm_xtversion[i] = '\0';
}

// Returns what cm reports for XTVERSION.
//
// A zero-length string makes libghostty report its own default, "libghostty", so an unset value
// degrades to the previous behavior rather than to an empty answer.
static GhosttyString cm_xtversion_cb(GhosttyTerminal terminal, void *userdata) {
  (void)terminal;
  (void)userdata;
  GhosttyString out;
  out.ptr = (const uint8_t *)cm_xtversion;
  size_t n = 0;
  while (cm_xtversion[n] != '\0') n++;
  out.len = n;
  return out;
}

GhosttyResult cm_install_callbacks(GhosttyTerminal terminal,
                                   uintptr_t handle,
                                   bool write_pty,
                                   bool title_changed,
                                   bool pwd_changed,
                                   bool xtversion) {
  GhosttyResult rc;

  // The handle identifies which Go terminal a callback belongs to. It is passed as the value
  // itself, and comes back to each callback as its userdata argument.
  //
  // A cgo.Handle is a small integer, not a pointer into Go memory, so libghostty can hold it
  // safely and the garbage collector has nothing to track.
  rc = ghostty_terminal_set(terminal, GHOSTTY_TERMINAL_OPT_USERDATA, (const void *)handle);
  if (rc != GHOSTTY_SUCCESS) return rc;

  // Callback options take the function pointer *itself* as the value, not a pointer to it. This
  // is the one asymmetry in the API worth knowing: userdata above is passed by address, callbacks
  // are passed directly. Passing &fn instead makes libghostty treat a stack address as the
  // function to call, which crashes inside vt_write with a program counter somewhere in the
  // stack. Confirmed against ghostty's own example/c-vt-effects.
  if (write_pty) {
    rc = ghostty_terminal_set(terminal, GHOSTTY_TERMINAL_OPT_WRITE_PTY,
                              (const void *)cmWritePty);
    if (rc != GHOSTTY_SUCCESS) return rc;
  }

  if (title_changed) {
    rc = ghostty_terminal_set(terminal, GHOSTTY_TERMINAL_OPT_TITLE_CHANGED,
                              (const void *)cmTitleChanged);
    if (rc != GHOSTTY_SUCCESS) return rc;
  }

  if (pwd_changed) {
    rc = ghostty_terminal_set(terminal, GHOSTTY_TERMINAL_OPT_PWD_CHANGED,
                              (const void *)cmPwdChanged);
    if (rc != GHOSTTY_SUCCESS) return rc;
  }

  // A C function rather than a Go trampoline, unlike the others. The callback returns a
  // GhosttyString whose memory must stay valid until it returns, and returning one built from Go
  // memory is exactly what cgo forbids.
  if (xtversion) {
    rc = ghostty_terminal_set(terminal, GHOSTTY_TERMINAL_OPT_XTVERSION,
                              (const void *)cm_xtversion_cb);
    if (rc != GHOSTTY_SUCCESS) return rc;
  }

  return GHOSTTY_SUCCESS;
}

GhosttyMode cm_mode_sync_output(void) { return GHOSTTY_MODE_SYNC_OUTPUT; }

GhosttyMode cm_mode_focus_event(void) { return GHOSTTY_MODE_FOCUS_EVENT; }
