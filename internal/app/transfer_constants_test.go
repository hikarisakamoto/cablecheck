package app

import (
	"encoding/base64"
	"testing"

	"cablecheck/internal/protocol"
	"cablecheck/internal/reporting"
)

// TestTransferConstantsPinnedToProtocol pins the reporting-layer transfer
// sizing constants equal to the protocol-layer ones. The two sets are declared
// separately on purpose — the reporting package must not import
// internal/protocol (the dependency-purity test forbids it) — but nothing else
// keeps them in step. Silent drift would either make chunks whose base64
// framing overruns protocol.MaxFrameSize (fatal write failures) or make the
// sender and receiver disagree on the caps (spurious manifest declines). The
// app package legitimately imports both, so this is where the contract is
// structurally pinned.
func TestTransferConstantsPinnedToProtocol(t *testing.T) {
	if reporting.ChunkSize != protocol.ChunkSize {
		t.Errorf("reporting.ChunkSize=%d != protocol.ChunkSize=%d", reporting.ChunkSize, protocol.ChunkSize)
	}
	if reporting.MaxTransferFileSize != protocol.MaxTransferFileSize {
		t.Errorf("reporting.MaxTransferFileSize=%d != protocol.MaxTransferFileSize=%d",
			reporting.MaxTransferFileSize, protocol.MaxTransferFileSize)
	}
	if reporting.MaxTransferTotal != protocol.MaxTransferTotal {
		t.Errorf("reporting.MaxTransferTotal=%d != protocol.MaxTransferTotal=%d",
			reporting.MaxTransferTotal, protocol.MaxTransferTotal)
	}
}

// TestChunkFitsFrame confirms a maximum-size chunk still fits a protocol frame
// after base64 expansion plus envelope overhead — the invariant that justifies
// declaring ChunkSize the value it is. If a future edit raised ChunkSize past
// ~700 KiB this would catch it before a peer fatally rejected the oversized
// frame.
func TestChunkFitsFrame(t *testing.T) {
	base64Len := base64.StdEncoding.EncodedLen(reporting.ChunkSize)
	const envelopeOverhead = 4096 // generous allowance for JSON keys and framing
	if base64Len+envelopeOverhead >= protocol.MaxFrameSize {
		t.Errorf("a ChunkSize chunk base64s to %d bytes; with overhead it does not fit the %d-byte frame cap",
			base64Len, protocol.MaxFrameSize)
	}
}
