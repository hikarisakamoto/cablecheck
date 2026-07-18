package peer

import "cablecheck/internal/protocol"

// event is one message on the session event loop's channel. The loop owns
// all session state; producers (connection reader, stdin pump, op executor)
// only ever send events, and every send selects on the session context so no
// producer can block after the loop exits (docs/design/proto.md §9).
type event interface{ isEvent() }

// evFrame delivers one inbound envelope from the connection reader.
type evFrame struct {
	env *protocol.Envelope
}

// evConnErr delivers the connection reader's terminal error; the reader
// exits immediately after sending it.
type evConnErr struct {
	err error
}

// evStdin delivers one line typed by the operator.
type evStdin struct {
	line string
}

// evStdinEOF signals end of stdin; the session treats it as "quit".
type evStdinEOF struct{}

// evOpDone delivers the worker executor's finished op: the test_result
// payload to send, correlated to the originating request's message ID.
type evOpDone struct {
	reqID         string
	res           protocol.TestResult
	resultPayload any
}

// evOpProgress delivers one throttled progress update from the worker
// executor, correlated to the originating request's message ID.
type evOpProgress struct {
	reqID string
	p     protocol.TestProgress
}

// evPlanDone delivers the coordinator plan driver's outcome: a nil err means
// the whole test sequence succeeded and the session may move on to report
// transfer and the complete exchange.
type evPlanDone struct {
	err error
}

// evTransferDone delivers a report-transfer callback's outcome. Transfer
// failures are warnings by contract — they never change the session outcome.
type evTransferDone struct {
	err error
}

// evCallExpired is sent by a Call whose budget (TimeoutMs + grace) ran out
// with no test_result: the coordinator aborts the session — a worker that
// heartbeats but cannot answer is not trustworthy for the remaining steps.
type evCallExpired struct {
	msgID string
	op    string
}

func (evFrame) isEvent()        {}
func (evConnErr) isEvent()      {}
func (evStdin) isEvent()        {}
func (evStdinEOF) isEvent()     {}
func (evOpDone) isEvent()       {}
func (evOpProgress) isEvent()   {}
func (evPlanDone) isEvent()     {}
func (evTransferDone) isEvent() {}
func (evCallExpired) isEvent()  {}

// Compile-time checks that every producer type satisfies event; the session
// event loop consumes these.
var _ = []event{
	evFrame{}, evConnErr{}, evStdin{}, evStdinEOF{}, evOpDone{}, evOpProgress{},
	evPlanDone{}, evTransferDone{}, evCallExpired{},
}
