#include "callbacks.h"
#include "_cgo_export.h"

#include <stdint.h>

GhosttyResult cm_install_callbacks(GhosttyTerminal terminal,
                                   uintptr_t handle,
                                   bool write_pty,
                                   bool title_changed,
                                   bool pwd_changed) {
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

  return GHOSTTY_SUCCESS;
}

GhosttyMode cm_mode_sync_output(void) { return GHOSTTY_MODE_SYNC_OUTPUT; }

GhosttyMode cm_mode_focus_event(void) { return GHOSTTY_MODE_FOCUS_EVENT; }
