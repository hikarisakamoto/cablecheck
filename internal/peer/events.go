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

func (evFrame) isEvent()      {}
func (evConnErr) isEvent()    {}
func (evStdin) isEvent()      {}
func (evStdinEOF) isEvent()   {}
func (evOpDone) isEvent()     {}
func (evOpProgress) isEvent() {}

// Compile-time checks that every producer type satisfies event; the session
// event loop consumes these.
var _ = []event{evFrame{}, evConnErr{}, evStdin{}, evStdinEOF{}, evOpDone{}, evOpProgress{}}
