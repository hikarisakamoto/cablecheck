package config

import "time"

// preset holds the per-mode default test parameters (design clieval.md §1.4).
// Fields left zero for a mode (soakDuration/soakLoad outside soak) are
// invalid to set in that mode rather than merely defaulted.
type preset struct {
	tcpDuration     time.Duration
	udpDuration     time.Duration
	parallelStreams int
	pingCount       int
	pingInterval    time.Duration
	tcpRepeats      int // in soak mode: repeats per cycle
	monitorInterval time.Duration
	soakDuration    time.Duration // soak mode only
	soakLoad        SoakLoad      // soak mode only
}

// presets maps each valid mode to its defaults. Membership in this map is
// also the mode-validity check performed first by Resolve.
var presets = map[Mode]preset{
	ModeQuick: {
		tcpDuration:     30 * time.Second,
		udpDuration:     20 * time.Second,
		parallelStreams: 4,
		pingCount:       500,
		pingInterval:    20 * time.Millisecond,
		tcpRepeats:      1,
		monitorInterval: time.Second, // link-state watch only in quick mode
	},
	ModeStandard: {
		tcpDuration:     60 * time.Second,
		udpDuration:     30 * time.Second,
		parallelStreams: 4,
		pingCount:       1500,
		pingInterval:    20 * time.Millisecond,
		tcpRepeats:      2,
		monitorInterval: time.Second,
	},
	ModeSoak: {
		tcpDuration:     60 * time.Second, // per cycle
		udpDuration:     20 * time.Second, // per cycle
		parallelStreams: 4,
		pingCount:       500, // per cycle
		pingInterval:    20 * time.Millisecond,
		tcpRepeats:      1, // per cycle
		monitorInterval: time.Second,
		soakDuration:    time.Hour,
		soakLoad:        SoakLoadPeriodic,
	},
}
