#include "callbacks.h"
#include "_cgo_export.h"

// Each returns the address of the Go trampoline. Kept in C because the generated header that
// declares them is only visible to C compiled as part of this package.
// cgo generates the trampoline with a non-const data pointer, while libghostty declares the
// callback with a const one. The cast is safe because the trampoline only reads the buffer,
// and it silences a warning that would otherwise be noise on every build.
GhosttyTerminalWritePtyFn cm_write_pty_fn(void) {
  return (GhosttyTerminalWritePtyFn)cmWritePty;
}
GhosttyTerminalTitleChangedFn cm_title_changed_fn(void) { return cmTitleChanged; }
GhosttyTerminalPwdChangedFn cm_pwd_changed_fn(void) { return cmPwdChanged; }

GhosttyMode cm_mode_sync_output(void) { return GHOSTTY_MODE_SYNC_OUTPUT; }
