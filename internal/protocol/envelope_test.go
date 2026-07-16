package protocol_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"cablecheck/internal/protocol"
)

// TestEnvelopeVersionMismatch checks CheckVersion: match passes, any other
// version yields a typed *VersionMismatchError carrying got and want.
func TestEnvelopeVersionMismatch(t *testing.T) {
	env, err := protocol.NewEnvelope(protocol.TypeHello, "", "pc2-00000001", nil)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	if env.ProtocolVersion != protocol.Version {
		t.Fatalf("NewEnvelope protocolVersion = %q, want %q", env.ProtocolVersion, protocol.Version)
	}
	if err := protocol.CheckVersion(env); err != nil {
		t.Fatalf("CheckVersion on matching version: %v", err)
	}

	env.ProtocolVersion = "2"
	err = protocol.CheckVersion(env)
	if err == nil {
		t.Fatal("CheckVersion accepted mismatched version")
	}
	var vm *protocol.VersionMismatchError
	if !errors.As(err, &vm) {
		t.Fatalf("CheckVersion error = %v (%T), want *protocol.VersionMismatchError", err, err)
	}
	if vm.Got != "2" {
		t.Errorf("VersionMismatchError.Got = %q, want %q", vm.Got, "2")
	}
	if vm.Want != protocol.Version {
		t.Errorf("VersionMismatchError.Want = %q, want %q", vm.Want, protocol.Version)
	}
	msg := err.Error()
	if !strings.Contains(msg, `"2"`) || !strings.Contains(msg, fmt.Sprintf("%q", protocol.Version)) {
		t.Errorf("error text %q does not mention both versions", msg)
	}
}

// TestDecodePayloadExplicitTypes exercises the generic DecodePayload: typed
// round-trip, error on absent payload, tolerance for unknown payload fields,
// and error on malformed JSON.
func TestDecodePayloadExplicitTypes(t *testing.T) {
	want := protocol.Hello{Token: "secret", Role: "pc2", CablecheckVersion: "1.0.0", LocalIP: "10.0.0.2", PeerIP: "10.0.0.1"}
	env, err := protocol.NewEnvelope(protocol.TypeHello, "", "pc2-00000001", want)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	got, err := protocol.DecodePayload[protocol.Hello](env)
	if err != nil {
		t.Fatalf("DecodePayload[Hello]: %v", err)
	}
	if *got != want {
		t.Errorf("DecodePayload[Hello] = %+v, want %+v", *got, want)
	}

	bare, err := protocol.NewEnvelope(protocol.TypeReady, "ct-x", "pc1-00000001", nil)
	if err != nil {
		t.Fatalf("NewEnvelope(nil payload): %v", err)
	}
	if _, err := protocol.DecodePayload[protocol.Ready](bare); !errors.Is(err, protocol.ErrNoPayload) {
		t.Fatalf("DecodePayload on absent payload = %v, want ErrNoPayload", err)
	}

	fwd := &protocol.Envelope{Payload: json.RawMessage(`{"code":"slow_link","text":"link slow","stage":"testing","futureField":42}`)}
	w, err := protocol.DecodePayload[protocol.Warning](fwd)
	if err != nil {
		t.Fatalf("DecodePayload with unknown payload field: %v", err)
	}
	if w.Code != "slow_link" || w.Text != "link slow" || w.Stage != "testing" {
		t.Errorf("Warning payload = %+v", *w)
	}

	bad := &protocol.Envelope{Payload: json.RawMessage(`{"code":`)}
	if _, err := protocol.DecodePayload[protocol.Warning](bad); err == nil {
		t.Fatal("DecodePayload accepted malformed payload JSON")
	}
}

// TestDuplicateMessageID checks the ID maker format and the duplicate
// detector: repeats and non-increasing sequence numbers are dropped with a
// reason, per-role tracking is independent, and dedup outlives the ring.
func TestDuplicateMessageID(t *testing.T) {
	maker := protocol.NewMessageIDMaker("pc1")
	if got := maker.Next(); got != "pc1-00000001" {
		t.Fatalf("first ID = %q, want %q", got, "pc1-00000001")
	}
	if got := maker.Next(); got != "pc1-00000002" {
		t.Fatalf("second ID = %q, want %q", got, "pc1-00000002")
	}

	det := protocol.NewDuplicateDetector()

	if ok, reason := det.Observe("pc1-00000001"); !ok || reason != "" {
		t.Fatalf("Observe(fresh) = %t, %q", ok, reason)
	}
	if ok, reason := det.Observe("pc1-00000001"); ok || reason == "" {
		t.Fatalf("Observe(repeat) = %t, %q; want drop with warning reason", ok, reason)
	}
	if ok, _ := det.Observe("pc1-00000002"); !ok {
		t.Fatal("Observe(next in sequence) dropped")
	}
	if ok, _ := det.Observe("pc1-00000005"); !ok {
		t.Fatal("Observe(seq jump forward) dropped; gaps must be allowed")
	}
	if ok, reason := det.Observe("pc1-00000003"); ok || reason == "" {
		t.Fatalf("Observe(non-increasing seq) = %t, %q; want drop with warning reason", ok, reason)
	}

	// Roles are tracked independently.
	if ok, _ := det.Observe("pc2-00000001"); !ok {
		t.Fatal("Observe(other role, seq 1) dropped; roles must be independent")
	}

	// Push far more than the 128-entry ring for pc2; the early ID must still
	// be caught via the per-role max sequence number.
	for i := 2; i <= 300; i++ {
		if ok, reason := det.Observe(fmt.Sprintf("pc2-%08d", i)); !ok {
			t.Fatalf("Observe(pc2 #%d) dropped: %s", i, reason)
		}
	}
	if ok, reason := det.Observe("pc2-00000001"); ok || reason == "" {
		t.Fatalf("Observe(repeat beyond ring) = %t, %q; want drop via max-seq", ok, reason)
	}

	// IDs that do not parse as "<role>-<seq>" are deduped by the ring alone.
	if ok, _ := det.Observe("bogus"); !ok {
		t.Fatal("Observe(unparseable, fresh) dropped")
	}
	if ok, reason := det.Observe("bogus"); ok || reason == "" {
		t.Fatalf("Observe(unparseable, repeat) = %t, %q; want drop via ring", ok, reason)
	}
}
