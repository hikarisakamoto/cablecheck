package evaluate

import (
	"cablecheck/internal/model"
)

// RulesVersion identifies the rule set implemented by this package; it is
// recorded in every evaluation so reports stay interpretable when thresholds
// change.
const RulesVersion = "1.1.0"

// Result is the complete output of the evaluation engine.
type Result struct {
	// Class is the overall health classification.
	Class model.HealthClass
	// Score is the 0-100 health score, clamped into the class band; nil for
	// INCONCLUSIVE (a number would imply confidence the evidence lacks).
	Score *int
	// Findings lists every rule finding in the fixed Rules() order.
	Findings []model.Finding
	// Recommendations are deduplicated, actionable follow-ups in finding
	// order.
	Recommendations []string
	// RulesVersion is the version of the rule set that produced the result.
	RulesVersion string
}

// Evaluate runs the fixed rule list over the facts and folds the findings
// into a classification, score and recommendations. It is pure and
// deterministic: identical facts always yield an identical result.
func Evaluate(f *Facts) Result {
	var findings []model.Finding
	for _, rule := range Rules() {
		if fd := rule.Evaluate(f); fd != nil {
			findings = append(findings, *fd)
		}
	}
	class := classify(findings, f)
	return Result{
		Class:           class,
		Score:           scoreFor(f, findings, class),
		Findings:        findings,
		Recommendations: recommend(findings, class),
		RulesVersion:    RulesVersion,
	}
}

// classify folds the findings into a health class.
//
// The asymmetry is deliberate: physical POOR/FAILED evidence is never
// softened by host evidence, while performance POOR is downgraded to
// INCONCLUSIVE when the host was limited and the physical layer is clean.
// Limitation caps only ever downgrade (EXCELLENT/GOOD toward
// INCONCLUSIVE/GOOD), never hide failure evidence.
func classify(findings []model.Finding, f *Facts) model.HealthClass {
	worstPhys := worstIn(findings, model.CategoryPhysical)
	hostLimited := hasFinding(findings, "HOST-01") || hasFinding(findings, "HOST-03")
	if worstPhys == model.SevFailed {
		return model.HealthFailed
	}
	if worstPhys == model.SevPoor {
		return model.HealthPoor
	}
	worstTP := worstIn(findings, model.CategoryTransport, model.CategoryPerformance)
	tentative := model.HealthExcellent
	switch {
	case worstTP >= model.SevPoor:
		if hostLimited && worstPhys < model.SevWarning && allPoorAreHostSensitive(findings) {
			return model.HealthInconclusive // host-limited, cable NOT proven bad
		}
		tentative = model.HealthPoor
	case worstTP == model.SevWarning || worstPhys == model.SevWarning:
		tentative = model.HealthWarning
	case anyInfoDeviation(findings):
		tentative = model.HealthGood
	}
	// Limitation caps: only ever downgrade, never upgrade.
	if hasFinding(findings, "HOST-02") {
		return model.HealthInconclusive
	}
	// LIM-05 joins the cap, not a hard gate, so a real physical failure (which
	// short-circuits above) still wins over an unreachable data port.
	if hasFinding(findings, "LIM-01") || hasFinding(findings, "LIM-03") || hasFinding(findings, "LIM-05") {
		if tentative == model.HealthExcellent || tentative == model.HealthGood {
			return model.HealthInconclusive
		}
	}
	if hasFinding(findings, "LIM-02") && tentative == model.HealthExcellent {
		tentative = model.HealthGood
	}
	return tentative
}

// worstIn returns the highest ladder severity (info..failed) among findings
// in the given categories; markers are outside the ladder and ignored. It
// returns -1 when no finding matches.
func worstIn(findings []model.Finding, cats ...model.Category) model.Severity {
	worst := model.Severity(-1)
	for _, fd := range findings {
		if fd.Severity >= model.SevMarker {
			continue
		}
		for _, cat := range cats {
			if fd.Category == cat && fd.Severity > worst {
				worst = fd.Severity
			}
		}
	}
	return worst
}

// hasFinding reports whether a finding with the given rule ID is present.
func hasFinding(findings []model.Finding, ruleID string) bool {
	for _, fd := range findings {
		if fd.RuleID == ruleID {
			return true
		}
	}
	return false
}

// allPoorAreHostSensitive reports whether every POOR-or-worse ladder finding
// is host-sensitive. A transport POOR (e.g. ping loss) is not host-sensitive
// and keeps the run POOR even with hot CPUs.
func allPoorAreHostSensitive(findings []model.Finding) bool {
	for _, fd := range findings {
		if fd.Severity >= model.SevPoor && fd.Severity < model.SevMarker && !fd.HostSensitive {
			return false
		}
	}
	return true
}

// anyInfoDeviation reports whether any physical/transport/performance finding
// carries an informational deviation (e.g. throughput 70-90% of link speed),
// which demotes EXCELLENT to GOOD.
func anyInfoDeviation(findings []model.Finding) bool {
	for _, fd := range findings {
		switch fd.Category {
		case model.CategoryPhysical, model.CategoryTransport, model.CategoryPerformance:
			if fd.Severity == model.SevInfo {
				return true
			}
		}
	}
	return false
}
