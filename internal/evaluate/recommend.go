package evaluate

import (
	"strings"

	"cablecheck/internal/model"
)

// Recommendation action texts. Several rules intentionally share an action.
const (
	recCable     = "Reseat both connectors and inspect for damage; replace the cable with a known-good Cat5e/Cat6 and rerun."
	recSpeed     = "Reduced link speed: inspect cable wiring and connectors, test with another cable, and verify both NICs advertise the expected speed (`ethtool <if>`)."
	recFlaky     = "Intermittent link: check connector seating, try a different NIC port, run `--mode soak` to catch drops."
	recCableTest = "Inspect the cable-test pair/status; replace or re-terminate the cable if the fault persists."
	recTCPRetest = "Retest with `--parallel-streams 1`; correlate with counter deltas and CPU before blaming the cable."
	recUDPRetest = "Rerun UDP at a lower `--udp-rate`; check NIC counter deltas and CPU before blaming the cable."
	recHost      = "Result appears host-limited: close background load, disable CPU power saving, avoid USB adapters, rerun."
	recCoverage  = "Restore the missing throughput or NIC-counter measurements and rerun; install iperf3/ethtool if unavailable."

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
	"PHY-08":  recCableTest,
	"PHY-09":  recCable,
	"PHY-10":  recCable,
	"PHY-11":  recCable,
	"TR-06":   recTCPRetest,
	"TR-07":   recUDPRetest,
	"HOST-01": recHost,
	"HOST-03": recHost,
	"HOST-04": recHost,
	"HOST-02": "Rerun on the physical interface — a virtual interface cannot exercise the cable.",
	"LIM-01":  recCoverage,
	"LIM-05":  "iperf3 could not connect to the peer on the data port — check the host firewall (ufw/firewalld) on the receiving side and confirm the data port is open.",
}

// recommend walks the findings in order, groups findings that map to the same
// action, and grounds each action in the findings' concrete evidence. Both
// action and evidence order follow finding order. The isolation-test line is
// appended when the class is POOR, FAILED or INCONCLUSIVE.
func recommend(findings []model.Finding, class model.HealthClass) []string {
	type group struct {
		action       string
		evidence     []string
		seenEvidence map[string]bool
	}

	var groups []group
	groupIndex := map[string]int{}
	for _, fd := range findings {
		action := recTexts[fd.RuleID]
		if action == "" {
			continue
		}
		idx, ok := groupIndex[action]
		if !ok {
			idx = len(groups)
			groupIndex[action] = idx
			groups = append(groups, group{action: action, seenEvidence: map[string]bool{}})
		}
		for _, evidence := range fd.Evidence {
			if evidence == "" || groups[idx].seenEvidence[evidence] {
				continue
			}
			groups[idx].seenEvidence[evidence] = true
			groups[idx].evidence = append(groups[idx].evidence, evidence)
		}
	}

	var out []string
	for _, rec := range groups {
		text := rec.action
		if len(rec.evidence) > 0 {
			text += " Evidence from this run: " + strings.Join(rec.evidence, "; ") + "."
		}
		out = append(out, text)
	}
	switch class {
	case model.HealthPoor, model.HealthFailed, model.HealthInconclusive:
		out = append(out, recIsolation)
	}
	return out
}
