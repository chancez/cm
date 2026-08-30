package server

import (
	"context"
	"testing"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// A client that does not resize the session must still leave the model knowing its cell size.
//
// Cell metrics and size are separate facts and only the second produces a resize, so recording the first
// as a side effect of the second loses it for every attach that does not reflow: a resume never does, and
// neither does a client that does not own sizing. What that costs is images, asymmetrically, which is why
// it read as a size-dependent bug. A placement that scrolled above the viewport is restored by cropping
// the source in pixels and needs a cell height; an unscrolled one does not. So small images appeared on
// every client while large ones, which scroll because they nearly fill the window, silently did not.
func TestANonResizingAttachStillRecordsCellMetrics(t *testing.T) {
	ctx := context.Background()
	mgr, _, _ := newTestManager(t, nil)
	svc := &Service{mgr: mgr}

	sess, term := sessionForResumeSizing(t)
	sess.resizePolicy = ResizeLeader

	// The wide window, which types and therefore owns sizing. 100 cols over 1000px and 40 rows over
	// 800px, so 10x20 per cell.
	wide := sess.reserveClient()
	if err := svc.sizeForAttach(ctx, sess, wide, &serverv1.Open{
		Rows: 40, Cols: 100, XPixel: 1000, YPixel: 800,
	}); err != nil {
		t.Fatalf("sizing the wide client: %v", err)
	}
	a, err := sess.attach(nil, wide)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(a)
	sess.noteClientInput(a.token)

	// The narrow window, which never types, so it takes no sizing and the session stays at 40x100. Its
	// cells are a different shape: 80 cols over 800px and 24 rows over 720px is 10x30.
	narrow := sess.reserveClient()
	if err := svc.sizeForAttach(ctx, sess, narrow, &serverv1.Open{
		Rows: 24, Cols: 80, XPixel: 800, YPixel: 720,
	}); err != nil {
		t.Fatalf("sizing the narrow client: %v", err)
	}

	// The metrics are the attaching client's own, recorded even though nothing reflowed.
	sess.mu.Lock()
	got := sess.gfxCellHeight
	sess.mu.Unlock()
	if got != 30 {
		t.Errorf("gfxCellHeight = %d, want 30 from the attaching client's window", got)
	}

	// And the session did not reflow to the narrow window, which is the whole reason sizeForAttach
	// returned early. Noting a cell size must not be a way in through the back door.
	if rows, cols := modelSizeOf(term); rows != 40 || cols != 100 {
		t.Errorf("model is %dx%d, want 40x100 held: noting a cell size reflowed the session", rows, cols)
	}
	// The cell dimensions the model was actually given, which is the pair that lets it place an image.
	term.mu.Lock()
	cw, ch := term.cellWidth, term.cellHeight
	term.mu.Unlock()
	if cw != 10 || ch != 30 {
		t.Errorf("model cell size = %dx%d, want 10x30", cw, ch)
	}
}
