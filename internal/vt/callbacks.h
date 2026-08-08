// Accessors for the Go callback trampolines.
//
// The trampolines themselves are Go functions exported with //export, and cgo emits their C
// declarations into a generated header. Declaring them again here would conflict with that,
// so instead these functions return their addresses, defined in callbacks.c where the
// generated header is available.
#ifndef CM_CALLBACKS_H
#define CM_CALLBACKS_H

#include <ghostty/vt.h>

GhosttyTerminalWritePtyFn cm_write_pty_fn(void);
GhosttyTerminalTitleChangedFn cm_title_changed_fn(void);
GhosttyTerminalPwdChangedFn cm_pwd_changed_fn(void);

// Modes are built by a static inline function that cgo cannot call, so the ones cm needs are
// exposed as ordinary functions.
GhosttyMode cm_mode_sync_output(void);

#endif
