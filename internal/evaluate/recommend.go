package evaluate

import "cablecheck/internal/model"

// Shared recommendation texts (several rules map to the same action).
const (
	recCable = "Reseat both connectors and inspect for damage; replace the cable with a known-good Cat5e/Cat6 and rerun."
	recSpeed = "Reduced link speed: 1000BASE-T needs all four pairs — test with another cable; verify both NICs advertise 1000 Mb/s (`ethtool <if>`)."
	recFlaky = "Intermittent link: check connector seating, try a different NIC port, run `--mode soak` to catch drops."

	// recIsolation is always appended when the verdict is POOR, FAILED or
	// INCONCLUSIVE: swapping one variable at a time separates cable faults
	// from machine faults.
	recIsolation = "Isolation test: same machines with a different cable, then the same cable between different machines."
)

// recTexts maps rule IDs to their recommendation. Rules without an entry
// contribute no recommendation of their own.
var recTexts = map[string]string{
	"PHY-01":  recFlaky,
	"PHY-02":  recCable,
	"PHY-03":  recFlaky,
	"PHY-04":  recFlaky,
	"PHY-05":  "Half duplex usually means autonegotiation failure: enable autoneg on both sides; replace the cable.",
	"PHY-06":  recSpeed,
	"PHY-07":  recSpeed,
	"PHY-08":  "Cable test reports an open/short pair — replace or re-terminate the cable (the fault distance is listed in the findings).",
	"PHY-09":  recCable,
	"PHY-10":  recCable,
	"TR-06":   "Retest with `--parallel-streams 1`; correlate with counter deltas and CPU before blaming the cable.",
	"TR-07":   "Retest with `--parallel-streams 1`; correlate with counter deltas and CPU before blaming the cable.",
	"HOST-01": "Result appears host-limited: close background load, disable CPU power saving, avoid USB adapters, rerun.",
	"HOST-03": "Result appears host-limited: close background load, disable CPU power saving, avoid USB adapters, rerun.",
	"HOST-02": "Rerun on the physical interface — a virtual interface cannot exercise the cable.",
	"LIM-01":  "Install the missing tools (iperf3/ethtool) and rerun for a conclusive result.",
	"LIM-05":  "iperf3 could not connect to the peer on the data port — check the host firewall (ufw/firewalld) on the receiving side and confirm the data port is open.",
}

// recommend walks the findings in order, collects their mapped
// recommendations deduplicated while preserving order, and appends the
// isolation-test line when the class is POOR, FAILED or INCONCLUSIVE.
func recommend(findings []model.Finding, class model.HealthClass) []string {
	var out []string
	seen := map[string]bool{}
	add := func(text string) {
		if text == "" || seen[text] {
			return
		}
		seen[text] = true
		out = append(out, text)
	}
	for _, fd := range findings {
		add(recTexts[fd.RuleID])
	}
	switch class {
	case model.HealthPoor, model.HealthFailed, model.HealthInconclusive:
		add(recIsolation)
	}
	return out
}
