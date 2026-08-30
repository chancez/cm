package fault

// The points, all of them, in one place.
//
// This list is the answer to "what can a test intervene in", and it is a list rather than a scattering of
// callback fields so that answering that question does not mean reading the whole server. Each entry says
// what the window is and which bug makes it worth having, because a point that corresponds to nothing that
// ever went wrong is noise, and noise is how a mechanism like this stops being trusted.
//
// Compiled into every build. Only the behavior is behind the tag, so a call site referring to a point
// builds either way and there is no second list to keep in step.
const (
	// AfterLogAppend is in the pump, between appending a chunk to the client log and advancing lastSeq.
	//
	// resumePoints documents this window and explains why no lock can close it: the pump appends before it
	// takes s.mu, so a chunk can be in the log while lastSeq does not account for it. Reading the pair in
	// the wrong order there would make a restarting server resume from a position whose bytes never
	// arrive. A delay here makes that ordering testable instead of argued about, and it is the window that
	// made TestLastSeqAdvancesWithOutput flaky under load.
	AfterLogAppend Point = "after-log-append"

	// BeforeModelFeed is in the pump, after clients have been sent a chunk and before the terminal model
	// consumes it.
	//
	// This is the model-lag window, and both bugs that live in it are recorded in docs/architecture.md: a
	// fresh attach streaming from the log's end skips what the model has not reached, and one streaming
	// from the model's raw position drops a sequence the model has consumed but cannot serialize. The
	// second reproduced about one attach in eight by chance. A delay here makes the lag as long as a test
	// needs, so an attach can be made to land inside it every time.
	BeforeModelFeed Point = "before-model-feed"

	// BeforeShimWrite is in Session.Write, before the request reaches the shim.
	//
	// For the failure nothing else can produce: a write to the pty that does not complete. The response's
	// Written count is discarded today, so a short write would truncate a program's input silently, and
	// there is no way to provoke one because os.File.Write loops until the buffer is consumed. An error
	// here exercises the path that a wedged shell would.
	BeforeShimWrite Point = "before-shim-write"

	// AfterAttachOpened is in the Attach handler, after Opened has been sent and before the stream starts.
	//
	// A client is at its most fragile here: it has been told the session exists and has not yet received
	// any of it. A pause lets a test do something to the session, end it, restart the server, resize it,
	// while a client sits in that gap. The follower-revives-the-session bug was found in exactly this
	// window, by accident.
	AfterAttachOpened Point = "after-attach-opened"

	// BeforeClientCanAnswer is in the Attach handler, after the client has a token and before attach makes it
	// eligible to answer a question.
	//
	// A session created by an attach starts its program while that attach is still completing, so a program
	// that queries immediately can find no client to ask: proxyQuery returns silently and the question is
	// gone. The window is milliseconds wide and a TUI asking for the background colour on startup lands in it,
	// which is how it was found, by a test that had to sleep before querying to stop losing the query
	// altogether.
	//
	// A pause here holds the client outside the answerer set for as long as a test needs, so the race becomes
	// an ordering the test controls rather than one it has to win.
	BeforeClientCanAnswer Point = "before-client-can-answer"

	// BeforeShimReady is in the shim's startup, before it binds its socket.
	//
	// The server waits ten seconds for a shim to become ready and then gives up. That timeout was measured
	// at 10.38s per attempt against 0.36s when a session reference was wrongly validated and the shim
	// exited before binding, and a session named `work` worked throughout, which is what hid it. A delay
	// here makes the slow path reachable without breaking the shim.
	BeforeShimReady Point = "before-shim-ready"
)

// points is every point, for validating a spec against something rather than accepting any string.
//
// Kept next to the declarations so adding a constant without adding it here is visible in one screen. A
// spec naming an unknown point is reported, because the alternative is a test that configures a fault
// against a typo and passes while injecting nothing.
var points = map[Point]bool{
	AfterLogAppend:        true,
	BeforeModelFeed:       true,
	BeforeShimWrite:       true,
	AfterAttachOpened:     true,
	BeforeShimReady:       true,
	BeforeClientCanAnswer: true,
}
