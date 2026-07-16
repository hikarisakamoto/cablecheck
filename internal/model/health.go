package model

import (
	"encoding/json"
	"fmt"
	"strings"
)

// HealthClass is the overall cable-health classification of a test run.
type HealthClass string

// The six health classes, ordered best to worst; INCONCLUSIVE means the
// evidence does not support a verdict about the cable (e.g. host-limited
// throughput on otherwise clean physical counters, or a virtual interface).
const (
	HealthExcellent    HealthClass = "EXCELLENT"
	HealthGood         HealthClass = "GOOD"
	HealthWarning      HealthClass = "WARNING"
	HealthPoor         HealthClass = "POOR"
	HealthFailed       HealthClass = "FAILED"
	HealthInconclusive HealthClass = "INCONCLUSIVE"
)

// Category groups findings by the layer of evidence they are based on.
type Category string

// Finding categories. Physical findings dominate classification; host and
// limitation findings are markers that can only soften or gate other rules.
const (
	CategoryPhysical    Category = "physical"
	CategoryTransport   Category = "transport"
	CategoryPerformance Category = "performance"
	CategoryHost        Category = "host"
	CategoryLimitation  Category = "limitation"
)

// Severity orders findings from informational to failure. It marshals to and
// from a JSON string ("info", "warning", "poor", "failed", "marker").
type Severity int

// Severity levels. SevInfo < SevWarning < SevPoor < SevFailed form the
// ordinal ladder; SevMarker is outside the ladder and tags host/limitation
// markers that gate or soften classification without contributing to it.
const (
	SevInfo Severity = iota
	SevWarning
	SevPoor
	SevFailed
	SevMarker
)

// severityNames maps each severity to its canonical JSON string.
var severityNames = map[Severity]string{
	SevInfo:    "info",
	SevWarning: "warning",
	SevPoor:    "poor",
	SevFailed:  "failed",
	SevMarker:  "marker",
}

// String returns the canonical lowercase name of the severity.
func (s Severity) String() string {
	if name, ok := severityNames[s]; ok {
		return name
	}
	return fmt.Sprintf("severity(%d)", int(s))
}

// MarshalJSON encodes the severity as its canonical string form.
func (s Severity) MarshalJSON() ([]byte, error) {
	name, ok := severityNames[s]
	if !ok {
		return nil, fmt.Errorf("unknown severity %d", int(s))
	}
	return json.Marshal(name)
}

// UnmarshalJSON decodes a severity from its string form, case-insensitively.
func (s *Severity) UnmarshalJSON(data []byte) error {
	var name string
	if err := json.Unmarshal(data, &name); err != nil {
		return fmt.Errorf("severity: %w", err)
	}
	for sev, canonical := range severityNames {
		if strings.EqualFold(name, canonical) {
			*s = sev
			return nil
		}
	}
	return fmt.Errorf("unknown severity %q", name)
}

// Finding is one piece of rule-derived evidence about the cable or the test
// environment, with the concrete observations that triggered it.
type Finding struct {
	// RuleID identifies the rule that produced the finding, e.g. "PHY-02".
	RuleID string `json:"ruleId"`
	// Category is the evidence layer the rule inspects.
	Category Category `json:"category"`
	// Severity marshals as a string ("info", "warning", "poor", "failed", "marker").
	Severity Severity `json:"severity"`
	// Text is the human-readable statement of the finding.
	Text string `json:"text"`
	// Evidence lists concrete observations, e.g. "pc2 enp3s0: rx_crc_errors +42 during test".
	Evidence []string `json:"evidence,omitempty"`
	// HostSensitive marks findings eligible for the host-limited override
	// (performance symptoms that a busy host can cause on a healthy cable).
	HostSensitive bool `json:"hostSensitive"`
}

// Evaluation is the complete output of the evaluation engine as recorded in
// the report: classification, optional score, findings and recommendations.
type Evaluation struct {
	// Class is the overall health classification.
	Class HealthClass `json:"class"`
	// Score is the 0-100 health score; nil for INCONCLUSIVE runs.
	Score *int `json:"score"`
	// Findings lists every rule finding in deterministic rule order.
	Findings []Finding `json:"findings,omitempty"`
	// Recommendations are deduplicated, actionable follow-ups.
	Recommendations []string `json:"recommendations,omitempty"`
	// RulesVersion identifies the rule set that produced this evaluation.
	RulesVersion string `json:"rulesVersion"`
}
