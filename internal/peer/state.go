// Package peer implements the control-plane session between the two
// cablecheck processes: the session state machine, the transport seam used to
// establish the control connection, and the synchronous handshake that
// authenticates the peers, pins the protocol version, assigns the test ID and
// exchanges capabilities. The session event loop, RPC and report transfer
// build on these pieces.
package peer

import (
	"fmt"
	"strings"
	"sync"
)

// State is one node of the session state machine (docs/design/proto.md §4).
type State string

// Session states, in lifecycle order. Completed, Aborted and Failed are
// terminal: no transition ever leaves them.
const (
	// StateInitializing is the initial state before preflight checks run.
	StateInitializing State = "initializing"
	// StatePreflight covers local dependency and interface checks.
	StatePreflight State = "preflight"
	// StateListening is PC1 waiting for PC2's TCP connection.
	StateListening State = "listening"
	// StateConnecting is PC2 dialing PC1.
	StateConnecting State = "connecting"
	// StateHandshake covers hello through the capabilities exchange.
	StateHandshake State = "handshake"
	// StateWaitingForLocalStart waits for the local operator (or
	// --non-interactive auto-start).
	StateWaitingForLocalStart State = "waiting_for_local_start"
	// StateWaitingForPeerStart waits for the peer's ready frame after the
	// local side started.
	StateWaitingForPeerStart State = "waiting_for_peer_start"
	// StateReady is both sides ready, synchronized start pending.
	StateReady State = "ready"
	// StateTesting is the test plan executing.
	StateTesting State = "testing"
	// StateGeneratingReport covers report writing and transfer.
	StateGeneratingReport State = "generating_report"
	// StateCompleted is the successful terminal state.
	StateCompleted State = "completed"
	// StateAborted is the terminal state after a local or peer abort.
	StateAborted State = "aborted"
	// StateFailed is the terminal state after an unrecoverable local error.
	StateFailed State = "failed"
)

// validTransitions is the authoritative transition table from
// docs/design/proto.md §4, with one reconciled addition: initializing may
// also move to aborted, so a Ctrl+C works from every non-terminal state.
// Terminal states map to empty lists. Note that the peer's ready arriving
// while in waiting_for_local_start does not transition; the session records a
// flag instead, keeping this table honest.
var validTransitions = map[State][]State{
	StateInitializing:         {StatePreflight, StateAborted, StateFailed},
	StatePreflight:            {StateListening, StateConnecting, StateFailed, StateAborted},
	StateListening:            {StateHandshake, StateAborted, StateFailed},
	StateConnecting:           {StateHandshake, StateAborted, StateFailed},
	StateHandshake:            {StateWaitingForLocalStart, StateAborted, StateFailed},
	StateWaitingForLocalStart: {StateWaitingForPeerStart, StateReady, StateAborted, StateFailed},
	StateWaitingForPeerStart:  {StateReady, StateAborted, StateFailed},
	StateReady:                {StateTesting, StateAborted, StateFailed},
	StateTesting:              {StateGeneratingReport, StateAborted, StateFailed},
	StateGeneratingReport:     {StateCompleted, StateAborted, StateFailed},
	StateCompleted:            {},
	StateAborted:              {},
	StateFailed:               {},
}

// Terminal reports whether s is a terminal state (no outgoing transitions).
func (s State) Terminal() bool {
	return len(validTransitions[s]) == 0
}

// ErrInvalidTransition reports a Transition call not allowed by the table.
type ErrInvalidTransition struct {
	// From is the state the machine was in.
	From State
	// To is the rejected target state.
	To State
}

// Error implements the error interface.
func (e *ErrInvalidTransition) Error() string {
	return fmt.Sprintf("peer: invalid state transition %s -> %s", e.From, e.To)
}

// ErrWrongState reports a Require call made outside the allowed states; it is
// the guard failure for inbound-message handlers and stdin commands.
type ErrWrongState struct {
	// Cur is the state the machine was actually in.
	Cur State
	// Want lists the states in which the operation is allowed.
	Want []State
}

// Error implements the error interface.
func (e *ErrWrongState) Error() string {
	names := make([]string, len(e.Want))
	for i, s := range e.Want {
		names[i] = string(s)
	}
	return fmt.Sprintf("peer: wrong state %s, want one of [%s]", e.Cur, strings.Join(names, " "))
}

// StateMachine tracks the session state and enforces the transition table.
// All methods are safe for concurrent use.
type StateMachine struct {
	mu       sync.Mutex
	cur      State
	onChange func(from, to State)
}

// NewStateMachine returns a machine starting in initial. onChange, if
// non-nil, is invoked after every successful transition with the old and new
// states; it runs under the machine's lock, so it must not re-enter the
// machine (it exists for logging).
func NewStateMachine(initial State, onChange func(from, to State)) *StateMachine {
	return &StateMachine{cur: initial, onChange: onChange}
}

// Current returns the current state.
func (m *StateMachine) Current() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cur
}

// Transition moves the machine to to if the table allows it, invoking the
// onChange hook on success. A disallowed move returns *ErrInvalidTransition
// and leaves the state untouched.
func (m *StateMachine) Transition(to State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, allowed := range validTransitions[m.cur] {
		if allowed == to {
			from := m.cur
			m.cur = to
			if m.onChange != nil {
				m.onChange(from, to)
			}
			return nil
		}
	}
	return &ErrInvalidTransition{From: m.cur, To: to}
}

// Require returns nil when the current state is one of states, and
// *ErrWrongState otherwise. Every inbound-message handler and stdin command
// starts with a Require guard.
func (m *StateMachine) Require(states ...State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range states {
		if m.cur == s {
			return nil
		}
	}
	return &ErrWrongState{Cur: m.cur, Want: states}
}
