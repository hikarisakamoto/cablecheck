package protocol

import (
	"encoding/json"
	"time"
)

// MessageType discriminates the payload carried by an Envelope.
type MessageType string

// Message types, in rough lifecycle order: handshake (hello, hello_ack,
// capabilities), readiness sync (ready, start_confirmation), orchestration
// (test_request, test_progress, test_result, warning, abort, heartbeat),
// report transfer (report, report_chunk, report_ack) and session end
// (complete).
const (
	// TypeHello is PC2's opening frame carrying the shared token (Hello).
	TypeHello MessageType = "hello"
	// TypeHelloAck is PC1's answer assigning the test ID (HelloAck).
	TypeHelloAck MessageType = "hello_ack"
	// TypeCapabilities carries tool/NIC capabilities, both directions
	// (Capabilities).
	TypeCapabilities MessageType = "capabilities"
	// TypeReady signals the local operator started the run (Ready).
	TypeReady MessageType = "ready"
	// TypeStartConfirm is PC1's synchronized-start countdown
	// (StartConfirmation).
	TypeStartConfirm MessageType = "start_confirmation"
	// TypeTestRequest asks the worker to run one operation (TestRequest).
	TypeTestRequest MessageType = "test_request"
	// TypeTestProgress streams progress for an in-flight op (TestProgress).
	TypeTestProgress MessageType = "test_progress"
	// TypeTestResult completes one op (TestResult).
	TypeTestResult MessageType = "test_result"
	// TypeWarning carries a non-fatal advisory (Warning).
	TypeWarning MessageType = "warning"
	// TypeAbort terminates the session (Abort).
	TypeAbort MessageType = "abort"
	// TypeHeartbeat is periodic liveness (Heartbeat).
	TypeHeartbeat MessageType = "heartbeat"
	// TypeReport opens the report transfer with a manifest (ReportManifest).
	TypeReport MessageType = "report"
	// TypeReportChunk carries one slice of a report file (ReportChunk).
	TypeReportChunk MessageType = "report_chunk"
	// TypeReportAck acknowledges one transferred file, or declines the
	// manifest (ReportAck).
	TypeReportAck MessageType = "report_ack"
	// TypeComplete carries the final verdict before disconnect (Complete).
	TypeComplete MessageType = "complete"
)

// Hello is PC2's first frame after connecting (PC2 → PC1).
type Hello struct {
	// Token is the shared secret, plaintext on the wire; the direct
	// trusted-link deployment model is documented. Never logged.
	Token string `json:"token"`
	// Role must be "pc2".
	Role string `json:"role"`
	// CablecheckVersion is the sender's build version.
	CablecheckVersion string `json:"cablecheckVersion"`
	// LocalIP is PC2's --local-ip (sanity check).
	LocalIP string `json:"localIp"`
	// PeerIP is PC2's --peer-ip; must equal PC1's local IP.
	PeerIP string `json:"peerIp"`
}

// HelloAck accepts a Hello and assigns the session's test ID (PC1 → PC2).
type HelloAck struct {
	// TestID is the assigned session ID, e.g. "ct-20260715-143205-a1b2c3d4";
	// all later envelopes must carry it.
	TestID string `json:"testId"`
	// CablecheckVersion is the sender's build version.
	CablecheckVersion string `json:"cablecheckVersion"`
}

// Iperf3Caps describes the local iperf3 binary's abilities; the effective
// session capability is the AND of both sides.
type Iperf3Caps struct {
	// Version is the parsed iperf3 version, e.g. "3.12".
	Version string `json:"version"`
	// JSON reports -J support (required capability).
	JSON bool `json:"json"`
	// Reverse reports -R support.
	Reverse bool `json:"reverse"`
	// Bidir reports --bidir support.
	Bidir bool `json:"bidir"`
	// GetServerOutput reports --get-server-output support.
	GetServerOutput bool `json:"getServerOutput"`
	// UDP reports -u support.
	UDP bool `json:"udp"`
	// OneOff reports -1 (one-off server) support.
	OneOff bool `json:"oneOff"`
}

// NICInfo summarizes the network interface under test.
type NICInfo struct {
	// Name is the interface name, e.g. "enp3s0".
	Name string `json:"name"`
	// Driver is the kernel driver, e.g. "r8169".
	Driver string `json:"driver"`
	// SpeedMbps is the negotiated link speed in Mbit/s.
	SpeedMbps int `json:"speedMbps"`
	// Duplex is "full" or "half".
	Duplex string `json:"duplex"`
	// MTU is the interface MTU in bytes.
	MTU int `json:"mtu"`
	// MAC is the hardware address.
	MAC string `json:"mac"`
}

// Capabilities is exchanged in both directions after hello_ack (PC2 first).
type Capabilities struct {
	// CablecheckVersion is the sender's build version.
	CablecheckVersion string `json:"cablecheckVersion"`
	// OS is GOOS/GOARCH, e.g. "linux/amd64".
	OS string `json:"os"`
	// Kernel is the kernel release (uname -r).
	Kernel string `json:"kernel"`
	// Iperf3 describes the local iperf3 binary.
	Iperf3 Iperf3Caps `json:"iperf3"`
	// EthtoolVersion is the detected ethtool version, if any.
	EthtoolVersion string `json:"ethtoolVersion"`
	// PingVariant identifies the ping implementation, e.g.
	// "iputils-20240117".
	PingVariant string `json:"pingVariant"`
	// NIC describes the interface under test.
	NIC NICInfo `json:"nic"`
	// SudoAvailable reports whether passwordless sudo works locally.
	SudoAvailable bool `json:"sudoAvailable"`
	// CableTestSupported reports ethtool --cable-test support on this NIC.
	CableTestSupported bool `json:"cableTestSupported"`
	// AcceptReportTransfer is false when --no-report-transfer was given.
	AcceptReportTransfer bool `json:"acceptReportTransfer"`
}

// Ready signals that the local side is ready to start testing.
type Ready struct {
	// NonInteractive reports whether readiness was automatic (no operator).
	NonInteractive bool `json:"nonInteractive"`
}

// StartConfirmation schedules the synchronized start (PC1 → PC2).
type StartConfirmation struct {
	// StartAt is the absolute start time, recorded in the report only —
	// never used as the countdown anchor (clock skew).
	StartAt time.Time `json:"startAt"`
	// StartInMs is authoritative: each side anchors its countdown at frame
	// ARRIVAL + StartInMs.
	StartInMs int `json:"startInMs"`
	// Mode is the test mode: quick, standard or soak.
	Mode string `json:"mode"`
	// Steps lists display names of the planned steps; drives "[1/8]"
	// numbering on PC2.
	Steps []string `json:"steps"`
}

// TestRequest asks the worker to execute one operation (PC1 → PC2).
type TestRequest struct {
	// Op names the operation, e.g. "iperf3_server_start",
	// "iperf3_client_run", "ping_run", "counters_snapshot",
	// "iperf3_server_stop", "cancel".
	Op string `json:"op"`
	// Params is the op-specific parameter struct owned by
	// internal/testsuite.
	Params json.RawMessage `json:"params"`
	// TimeoutMs is the worker-side budget; the coordinator waits this plus
	// grace.
	TimeoutMs int `json:"timeoutMs"`
	// Step is this request's 1-based position in the plan.
	Step int `json:"step"`
	// TotalSteps is the plan length.
	TotalSteps int `json:"totalSteps"`
}

// TestProgress streams progress for an in-flight op (PC2 → PC1);
// Envelope.InReplyTo carries the request's MessageID.
type TestProgress struct {
	// Stage names the current phase of the op.
	Stage string `json:"stage"`
	// Percent is completion in [0,100]; -1 means indeterminate.
	Percent float64 `json:"percent"`
	// Text is a human-readable progress line.
	Text string `json:"text"`
	// Metrics carries live numbers, e.g. {"bitrateMbps": 941.2}.
	Metrics map[string]float64 `json:"metrics,omitempty"`
}

// TestResult completes one op (PC2 → PC1); Envelope.InReplyTo carries the
// request's MessageID.
type TestResult struct {
	// Status is "ok", "failed", "timeout", "unavailable" or "rejected".
	Status string `json:"status"`
	// Result is the typed per-op result struct (internal/model), when any.
	Result json.RawMessage `json:"result,omitempty"`
	// Error describes the failure for non-ok statuses.
	Error string `json:"error,omitempty"`
	// StartedAt is when the worker began the op.
	StartedAt time.Time `json:"startedAt"`
	// FinishedAt is when the worker finished the op.
	FinishedAt time.Time `json:"finishedAt"`
}

// Warning carries a non-fatal advisory, either direction.
type Warning struct {
	// Code is a stable machine-readable identifier, e.g. "invalid_state".
	Code string `json:"code"`
	// Text is the human-readable message.
	Text string `json:"text"`
	// Stage is the state or op during which the warning arose.
	Stage string `json:"stage"`
}

// Abort terminates the session; the sender closes after sending and never
// waits for a reply.
type Abort struct {
	// Reason is one of "user_interrupt", "auth_failed", "version_mismatch",
	// "protocol_error", "request_timeout", "capability_missing",
	// "internal_error".
	Reason string `json:"reason"`
	// Stage is the state-machine state or op name at abort time.
	Stage string `json:"stage"`
	// Detail optionally elaborates (never for auth_failed).
	Detail string `json:"detail,omitempty"`
	// Initiator is "pc1" or "pc2".
	Initiator string `json:"initiator"`
}

// Heartbeat is the periodic liveness frame sent by both sides.
type Heartbeat struct {
	// Seq increments per heartbeat sent.
	Seq uint64 `json:"seq"`
	// State is the sender's state-machine state (feeds the status command).
	State string `json:"state"`
	// ActiveOp names the op in flight, if any.
	ActiveOp string `json:"activeOp,omitempty"`
}

// ReportFile describes one file in a ReportManifest.
type ReportFile struct {
	// Name is the bare file name; the receive-side allowlist is exactly
	// {"report.json", "report.md", "summary.txt"}.
	Name string `json:"name"`
	// Size is the file size in bytes.
	Size int64 `json:"size"`
	// SHA256 is the hex-encoded digest of the whole file.
	SHA256 string `json:"sha256"`
}

// ReportManifest opens the report transfer (message type "report").
type ReportManifest struct {
	// Files lists the files about to be streamed, in transfer order.
	Files []ReportFile `json:"files"`
	// TotalSize is the sum of all file sizes in bytes.
	TotalSize int64 `json:"totalSize"`
}

// ReportChunk carries one slice of a report file (PC1 → PC2).
type ReportChunk struct {
	// Name is the manifest file name this chunk belongs to.
	Name string `json:"name"`
	// Seq is the 0-based chunk index per file; must be contiguous.
	Seq int `json:"seq"`
	// Offset must equal the receiver's bytes-received count for the file.
	Offset int64 `json:"offset"`
	// Data is the raw chunk (encoding/json base64s it automatically);
	// at most ChunkSize bytes.
	Data []byte `json:"data"`
	// Last marks the final chunk of the file.
	Last bool `json:"last"`
}

// ReportAck acknowledges one transferred file — or declines the whole
// manifest when Name is empty and Declined is true (PC2 → PC1).
type ReportAck struct {
	// Name is the acknowledged file, or "" for a manifest-level decline.
	Name string `json:"name"`
	// OK reports whether the file arrived intact (digest verified).
	OK bool `json:"ok"`
	// Declined is set on a manifest-level refusal.
	Declined bool `json:"declined,omitempty"`
	// Error describes why OK is false or the manifest was declined.
	Error string `json:"error,omitempty"`
}

// Complete carries the final verdict before orderly disconnect.
type Complete struct {
	// Classification is the health class, EXCELLENT through INCONCLUSIVE.
	Classification string `json:"classification"`
	// Summary is the one-line verdict.
	Summary string `json:"summary"`
	// ExitCode is PC1's process exit code.
	ExitCode int `json:"exitCode"`
}
