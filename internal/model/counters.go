package model

import "time"

// CounterSnapshot is one point-in-time capture of a peer's NIC counters.
//
// A key that is absent from Standard or Driver means "no data for this
// counter", which is NOT the same as zero: many drivers (Realtek, virtio)
// simply do not expose e.g. a CRC counter, and the evaluator must see
// "unavailable" rather than "0 errors".
type CounterSnapshot struct {
	// CapturedAt is when the snapshot was taken.
	CapturedAt time.Time `json:"capturedAt"`
	// Standard holds the normalized counter view keyed by standard names
	// such as "rx_crc", "rx_missed", "link_resets".
	Standard map[string]uint64 `json:"standard"`
	// Driver holds the full driver-specific `ethtool -S` counters, untouched.
	// Empty when ethtool statistics are unavailable.
	Driver map[string]uint64 `json:"driver"`
	// Raw is the verbatim `ethtool -S` text output for forensics.
	Raw string `json:"raw"`
	// IPStats is the full `ip -j -s -s` stats64 snapshot; filled even when
	// ethtool is unavailable.
	IPStats IPStats64 `json:"ipStats"`
}

// IPStats64 mirrors the `stats64` object of `ip -j -s -s link show` output.
// The detailed error counters are FLAT inside rx/tx (verified against live
// iproute2), so the JSON tags below match the tool's field names exactly.
type IPStats64 struct {
	// RX holds the receive-side counters.
	RX IPRxStats `json:"rx"`
	// TX holds the transmit-side counters.
	TX IPTxStats `json:"tx"`
}

// IPRxStats holds the receive counters of `ip -j -s -s`. The fields from
// LengthErrors down are present only with the double `-s` flag.
type IPRxStats struct {
	// Bytes is the total bytes received.
	Bytes uint64 `json:"bytes"`
	// Packets is the total packets received.
	Packets uint64 `json:"packets"`
	// Errors is the total receive errors.
	Errors uint64 `json:"errors"`
	// Dropped is the packets dropped on receive.
	Dropped uint64 `json:"dropped"`
	// OverErrors counts receiver ring-buffer overruns.
	OverErrors uint64 `json:"over_errors"`
	// Multicast counts received multicast packets.
	Multicast uint64 `json:"multicast"`
	// LengthErrors counts frames with bad length fields.
	LengthErrors uint64 `json:"length_errors"`
	// CRCErrors counts frames failing the CRC check.
	CRCErrors uint64 `json:"crc_errors"`
	// FrameErrors counts framing/alignment errors.
	FrameErrors uint64 `json:"frame_errors"`
	// FifoErrors counts FIFO overrun errors.
	FifoErrors uint64 `json:"fifo_errors"`
	// MissedErrors counts packets missed by the NIC.
	MissedErrors uint64 `json:"missed_errors"`
	// Nohandler counts packets with no protocol handler (emitted by the
	// tool only when nonzero).
	Nohandler uint64 `json:"nohandler"`
}

// IPTxStats holds the transmit counters of `ip -j -s -s`. The fields from
// AbortedErrors down are present only with the double `-s` flag.
type IPTxStats struct {
	// Bytes is the total bytes transmitted.
	Bytes uint64 `json:"bytes"`
	// Packets is the total packets transmitted.
	Packets uint64 `json:"packets"`
	// Errors is the total transmit errors.
	Errors uint64 `json:"errors"`
	// Dropped is the packets dropped on transmit.
	Dropped uint64 `json:"dropped"`
	// CarrierErrors counts carrier losses during transmit.
	CarrierErrors uint64 `json:"carrier_errors"`
	// Collisions counts transmit collisions (half-duplex evidence).
	Collisions uint64 `json:"collisions"`
	// AbortedErrors counts aborted transmissions.
	AbortedErrors uint64 `json:"aborted_errors"`
	// FifoErrors counts transmit FIFO errors.
	FifoErrors uint64 `json:"fifo_errors"`
	// WindowErrors counts late-collision window errors.
	WindowErrors uint64 `json:"window_errors"`
	// HeartbeatErrors counts heartbeat errors (legacy).
	HeartbeatErrors uint64 `json:"heartbeat_errors"`
	// CarrierChanges counts carrier up/down transitions; iproute2 reports
	// it under tx (verified live).
	CarrierChanges uint64 `json:"carrier_changes"`
}

// CounterDelta is the change of a single counter across the test run. Delta
// is meaningful only when OK is true; OK is false when the counter reset or
// wrapped (after < before) or when either capture lacked the key.
type CounterDelta struct {
	// Delta is the non-negative increase during the run.
	Delta uint64 `json:"delta"`
	// OK is false when the delta is unreliable (reset/wrap/missing capture).
	OK bool `json:"ok"`
}

// CounterDeltaSet maps a counter name to its delta across the test run.
// Absent keys mean the counter was unavailable on this hardware.
type CounterDeltaSet map[string]CounterDelta

// PeerCounters holds one counter snapshot per peer; a nil side means the
// capture did not happen (e.g. aborted run).
type PeerCounters struct {
	// PC1 is the snapshot taken on PC1.
	PC1 *CounterSnapshot `json:"pc1,omitempty"`
	// PC2 is the snapshot taken on PC2.
	PC2 *CounterSnapshot `json:"pc2,omitempty"`
}

// PeerCounterDeltas holds the per-peer counter deltas across the test run.
type PeerCounterDeltas struct {
	// PC1 is the delta set computed from PC1's snapshots.
	PC1 CounterDeltaSet `json:"pc1,omitempty"`
	// PC2 is the delta set computed from PC2's snapshots.
	PC2 CounterDeltaSet `json:"pc2,omitempty"`
}
