// Package protocol implements the framed JSON control protocol spoken
// between the two cablecheck peers: 4-byte big-endian length prefix followed
// by a JSON envelope. It provides the message catalog, envelope
// construction and typed payload decoding, the framed connection wrapper,
// message-ID generation with duplicate detection, and the constant-time
// token comparison used during the handshake.
package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cablecheck/internal/clock"
)

// wallClock is the package's single indirection to wall-clock time (kept in
// internal/clock per project policy). It stamps envelopes and anchors
// connection deadlines; both are inherently wall-clock concerns.
var wallClock clock.Clock = clock.Real{}

// Envelope is the wire representation of every protocol message: the JSON
// body of one frame. Unknown JSON fields are tolerated when decoding for
// forward compatibility.
type Envelope struct {
	// ProtocolVersion is the sender's protocol version; it must equal
	// Version exactly (no negotiation in v1).
	ProtocolVersion string `json:"protocolVersion"`
	// TestID identifies the session; empty until hello_ack assigns it.
	TestID string `json:"testId"`
	// MessageID is the sender-unique ID, "<role>-<8-digit seq>", e.g.
	// "pc2-00000042".
	MessageID string `json:"messageId"`
	// InReplyTo carries the MessageID being answered (RPC correlation).
	InReplyTo string `json:"inReplyTo,omitempty"`
	// Type selects which payload struct Payload decodes into.
	Type MessageType `json:"type"`
	// Timestamp is the sender's UTC send time (RFC 3339 with nanoseconds).
	Timestamp time.Time `json:"timestamp"`
	// Payload is the type-specific message body, absent for bare messages.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// NewEnvelope builds an Envelope of type t carrying payload, stamped with
// the current UTC time and this build's protocol Version. testID may be
// empty before hello_ack assigns one. A nil payload produces a bare
// envelope; any other value is JSON-marshaled into Envelope.Payload.
func NewEnvelope(t MessageType, testID, msgID string, payload any) (*Envelope, error) {
	env := &Envelope{
		ProtocolVersion: Version,
		TestID:          testID,
		MessageID:       msgID,
		Type:            t,
		Timestamp:       wallClock.Now().UTC(),
	}
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("protocol: marshal %s payload: %w", t, err)
		}
		env.Payload = raw
	}
	return env, nil
}

// ErrNoPayload is returned by DecodePayload when the envelope carries no
// payload.
var ErrNoPayload = errors.New("protocol: envelope has no payload")

// DecodePayload decodes env.Payload into a freshly allocated T. It fails
// with ErrNoPayload when the payload is absent and with a wrapped JSON
// error when it is malformed. Unknown payload fields are tolerated for
// forward compatibility. Payloads are always decoded into the explicit
// structs of this package — never into maps or arbitrary types.
func DecodePayload[T any](env *Envelope) (*T, error) {
	v := new(T)
	if env == nil || len(env.Payload) == 0 || string(env.Payload) == "null" {
		return nil, fmt.Errorf("protocol: decode %T: %w", *v, ErrNoPayload)
	}
	if err := json.Unmarshal(env.Payload, v); err != nil {
		return nil, fmt.Errorf("protocol: decode %T payload: %w", *v, err)
	}
	return v, nil
}

// VersionMismatchError reports that a peer announced a different protocol
// version. Exact match is required; there is no negotiation in v1.
type VersionMismatchError struct {
	// Got is the version announced by the peer.
	Got string
	// Want is the version this build speaks (Version).
	Want string
}

// Error implements the error interface.
func (e *VersionMismatchError) Error() string {
	return fmt.Sprintf("protocol version mismatch: peer speaks %q, we speak %q", e.Got, e.Want)
}

// CheckVersion validates env.ProtocolVersion against Version, returning a
// *VersionMismatchError on any difference.
func CheckVersion(env *Envelope) error {
	if env.ProtocolVersion != Version {
		return &VersionMismatchError{Got: env.ProtocolVersion, Want: Version}
	}
	return nil
}

// MessageIDMaker mints role-prefixed monotonic message IDs of the form
// "<role>-00000042" (one atomic counter per session): cheap, sortable in
// logs, and the input to duplicate detection on the receiving side. Next is
// safe for concurrent use.
type MessageIDMaker struct {
	role string
	seq  atomic.Uint64
}

// NewMessageIDMaker returns a maker whose IDs carry the given role prefix
// (normally "pc1" or "pc2"). The first ID has sequence number 1.
func NewMessageIDMaker(role string) *MessageIDMaker {
	return &MessageIDMaker{role: role}
}

// Next returns the next message ID.
func (m *MessageIDMaker) Next() string {
	return fmt.Sprintf("%s-%08d", m.role, m.seq.Add(1))
}

// dedupRingSize is the number of recently seen message IDs remembered
// verbatim for exact-duplicate detection.
const dedupRingSize = 128

// DuplicateDetector spots replayed and out-of-order message IDs on the
// receive path. It keeps the last dedupRingSize IDs verbatim plus the
// maximum sequence number seen per role prefix, so a repeat is caught even
// after it falls out of the ring. TCP already prevents duplication on the
// wire; this guards against application bugs and costs nothing. Safe for
// concurrent use.
type DuplicateDetector struct {
	mu     sync.Mutex
	ring   [dedupRingSize]string
	next   int
	maxSeq map[string]uint64
}

// NewDuplicateDetector returns an empty detector.
func NewDuplicateDetector() *DuplicateDetector {
	return &DuplicateDetector{maxSeq: make(map[string]uint64)}
}

// Observe records messageID and reports whether the frame should be
// processed. ok == false means the frame must be dropped and the returned
// reason logged as a warning: the ID was either seen recently or its
// sequence number does not increase for its role. IDs that do not parse as
// "<role>-<seq>" are deduplicated by the ring alone.
func (d *DuplicateDetector) Observe(messageID string) (ok bool, reason string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, seen := range d.ring {
		if seen == messageID && seen != "" {
			return false, fmt.Sprintf("duplicate messageId %q", messageID)
		}
	}
	if role, seq, parsed := splitMessageID(messageID); parsed {
		if last, tracked := d.maxSeq[role]; tracked && seq <= last {
			return false, fmt.Sprintf("non-increasing messageId %q: sequence %d after %d", messageID, seq, last)
		}
		d.maxSeq[role] = seq
	}
	d.ring[d.next] = messageID
	d.next = (d.next + 1) % dedupRingSize
	return true, ""
}

// splitMessageID parses "<role>-<seq>" IDs as minted by MessageIDMaker,
// splitting on the last hyphen so roles containing hyphens still parse.
func splitMessageID(id string) (role string, seq uint64, ok bool) {
	i := strings.LastIndexByte(id, '-')
	if i <= 0 || i == len(id)-1 {
		return "", 0, false
	}
	n, err := strconv.ParseUint(id[i+1:], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return id[:i], n, true
}
