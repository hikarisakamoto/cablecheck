package peer

import (
	"testing"
	"time"

	"cablecheck/internal/clock"
	"cablecheck/internal/protocol"
)

// TestConfigDefaults pins the zero-value defaulting of the test-tunable
// knobs and the injectable dependencies.
func TestConfigDefaults(t *testing.T) {
	var cfg Config
	if got := cfg.heartbeatInterval(); got != protocol.HeartbeatInterval {
		t.Errorf("heartbeatInterval() = %v, want protocol.HeartbeatInterval %v", got, protocol.HeartbeatInterval)
	}
	if got := cfg.idleTimeout(); got != protocol.DefaultIdleTimeout {
		t.Errorf("idleTimeout() = %v, want protocol.DefaultIdleTimeout %v", got, protocol.DefaultIdleTimeout)
	}
	// The RPC grace has its own constant: it must not be coupled to the
	// frame-write timeout, whose retuning would otherwise silently change
	// RPC deadlines (docs/design/proto.md §6 fixes the grace at 10s).
	if defaultCallGrace != 10*time.Second {
		t.Errorf("defaultCallGrace = %v, want the 10s grace of docs/design/proto.md §6", defaultCallGrace)
	}
	if got := cfg.callGrace(); got != defaultCallGrace {
		t.Errorf("callGrace() = %v, want defaultCallGrace %v", got, defaultCallGrace)
	}
	if cfg.clock() == nil {
		t.Error("clock() = nil, want the real clock by default")
	}
	if _, ok := cfg.clock().(clock.Real); !ok {
		t.Errorf("clock() = %T, want clock.Real", cfg.clock())
	}
	if cfg.logger() == nil {
		t.Error("logger() = nil, want a discard logger by default")
	}
	if _, ok := cfg.transport().(*tcpTransport); !ok {
		t.Errorf("transport() = %T, want *tcpTransport", cfg.transport())
	}

	// Explicit values win over defaults.
	cfg.HeartbeatInterval = 1
	cfg.IdleTimeout = 2
	cfg.CallGrace = 3
	if cfg.heartbeatInterval() != 1 || cfg.idleTimeout() != 2 || cfg.callGrace() != 3 {
		t.Errorf("explicit durations not honored: %v %v %v",
			cfg.heartbeatInterval(), cfg.idleTimeout(), cfg.callGrace())
	}
	tr := newPipeTransport()
	cfg.Transport = tr
	if cfg.transport() != Transport(tr) {
		t.Errorf("transport() = %v, want the injected transport", cfg.transport())
	}
}
