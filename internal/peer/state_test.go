package peer

import (
	"errors"
	"testing"
)

// allStates lists every session state, mirroring docs/design/proto.md §4.
var allStates = []State{
	StateInitializing,
	StatePreflight,
	StateListening,
	StateConnecting,
	StateHandshake,
	StateWaitingForLocalStart,
	StateWaitingForPeerStart,
	StateReady,
	StateTesting,
	StateGeneratingReport,
	StateCompleted,
	StateAborted,
	StateFailed,
}

// expectedTransitions restates the proto.md §4 table independently of the
// implementation's validTransitions map, so drift in either is caught.
var expectedTransitions = map[State][]State{
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

func containsState(list []State, s State) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// TestTransitionTable walks the full (from, to) matrix and asserts every pair
// is allowed or denied exactly per the design table, including that all three
// terminal states reject every outgoing transition.
func TestTransitionTable(t *testing.T) {
	if len(allStates) != 13 {
		t.Fatalf("expected 13 states, have %d", len(allStates))
	}
	for _, from := range allStates {
		for _, to := range allStates {
			want := containsState(expectedTransitions[from], to)
			sm := NewStateMachine(from, nil)
			err := sm.Transition(to)
			if want {
				if err != nil {
					t.Errorf("Transition(%s -> %s): unexpected error %v", from, to, err)
					continue
				}
				if got := sm.Current(); got != to {
					t.Errorf("Transition(%s -> %s): Current() = %s, want %s", from, to, got, to)
				}
				continue
			}
			if err == nil {
				t.Errorf("Transition(%s -> %s): allowed, want rejection", from, to)
				continue
			}
			var inv *ErrInvalidTransition
			if !errors.As(err, &inv) {
				t.Errorf("Transition(%s -> %s): error %v is not *ErrInvalidTransition", from, to, err)
				continue
			}
			if inv.From != from || inv.To != to {
				t.Errorf("Transition(%s -> %s): error carries From=%s To=%s", from, to, inv.From, inv.To)
			}
			if got := sm.Current(); got != from {
				t.Errorf("Transition(%s -> %s): rejected transition moved Current() to %s", from, to, got)
			}
		}
	}
	// Terminal states must have no outgoing transitions at all.
	for _, s := range []State{StateCompleted, StateAborted, StateFailed} {
		if !s.Terminal() {
			t.Errorf("State(%s).Terminal() = false, want true", s)
		}
		if n := len(expectedTransitions[s]); n != 0 {
			t.Errorf("terminal state %s has %d expected transitions in the test table", s, n)
		}
	}
}

// TestAbortFromEveryNonTerminalState asserts every non-terminal state may
// transition directly to aborted (Ctrl+C must work everywhere).
func TestAbortFromEveryNonTerminalState(t *testing.T) {
	for _, from := range allStates {
		if from.Terminal() {
			continue
		}
		sm := NewStateMachine(from, nil)
		if err := sm.Transition(StateAborted); err != nil {
			t.Errorf("Transition(%s -> aborted): %v, want success", from, err)
		}
	}
}

// TestStateMachineOnChange asserts the hook fires exactly once per successful
// transition with the right pair, and never on a rejected one.
func TestStateMachineOnChange(t *testing.T) {
	type change struct{ from, to State }
	var calls []change
	sm := NewStateMachine(StateInitializing, func(from, to State) {
		calls = append(calls, change{from, to})
	})
	if err := sm.Transition(StatePreflight); err != nil {
		t.Fatalf("Transition(initializing -> preflight): %v", err)
	}
	if err := sm.Transition(StateCompleted); err == nil {
		t.Fatal("Transition(preflight -> completed): allowed, want rejection")
	}
	if len(calls) != 1 || calls[0] != (change{StateInitializing, StatePreflight}) {
		t.Errorf("onChange calls = %+v, want exactly [{initializing preflight}]", calls)
	}
}

// TestRequire asserts Require passes when the current state is listed and
// returns a typed *ErrWrongState otherwise.
func TestRequire(t *testing.T) {
	sm := NewStateMachine(StateReady, nil)
	if err := sm.Require(StateTesting, StateReady); err != nil {
		t.Errorf("Require(testing, ready) in ready: %v, want nil", err)
	}
	err := sm.Require(StateTesting, StateHandshake)
	if err == nil {
		t.Fatal("Require(testing, handshake) in ready: nil, want error")
	}
	var wrong *ErrWrongState
	if !errors.As(err, &wrong) {
		t.Fatalf("Require error %v is not *ErrWrongState", err)
	}
	if wrong.Cur != StateReady || len(wrong.Want) != 2 {
		t.Errorf("ErrWrongState = %+v, want Cur=ready Want=[testing handshake]", wrong)
	}
}
