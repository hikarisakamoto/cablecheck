package evaluate

import "cablecheck/internal/model"

// classifyThroughput applies the speed-selected PERF-01 policy. The returned
// severity is meaningful only when deviation is true. Unknown negotiated
// speed and passing throughput both produce no deviation.
func classifyThroughput(measured, negotiated model.Bitrate, thresholds Thresholds) (ratio float64, severity model.Severity, deviation bool) {
	bands, ok := thresholds.perfBands(negotiated)
	if !ok {
		return 0, 0, false
	}
	ratio = float64(measured) / float64(negotiated)
	switch {
	case ratio >= bands.PassAt && !bands.PassDisabled:
		return ratio, 0, false
	case ratio >= bands.InfoAt:
		return ratio, model.SevInfo, true
	case ratio >= bands.WarningAt:
		return ratio, model.SevWarning, true
	default:
		return ratio, model.SevPoor, true
	}
}

// throughputShortfall reports whether a single trial fell short of the
// speed-selected pass ratio. Unlike classifyThroughput's deviation flag, a
// trial at or above PassAt is never a shortfall even on the <=100M tier, which
// annotates line-rate results as INFO because it never passes silently
// (PassDisabled). The isolated-outlier cap must count only genuine
// under-policy trials, so a healthy 100M trial does not mask a lone bad pass.
func throughputShortfall(measured, negotiated model.Bitrate, thresholds Thresholds) bool {
	bands, ok := thresholds.perfBands(negotiated)
	if !ok {
		return false
	}
	return float64(measured)/float64(negotiated) < bands.PassAt
}

// assessThroughput applies the isolated-outlier cap to one direction after
// classifying its aggregate bitrate. A single deviating trial can reduce a
// POOR aggregate only to WARNING, and only when every physical rule passes on
// reliable counter evidence. Other directions are assessed independently.
func assessThroughput(f *Facts, direction int, thresholds Thresholds) (ratio float64, severity model.Severity, deviation, capped bool) {
	d := &f.Dir[direction]
	ratio, severity, deviation = classifyThroughput(d.TCPBitrate, f.NegotiatedSpeed, thresholds)
	if !deviation || severity <= model.SevWarning {
		return ratio, severity, deviation, false
	}
	isolated := d.TCPTrialCount >= 2 && d.TCPThroughputDeviations == 1
	if isolated && physicalLayerClean(f, thresholds) {
		return ratio, model.SevWarning, true, true
	}
	return ratio, severity, true, false
}

// physicalLayerClean requires trustworthy counter captures, receive-error
// evidence from both peers, and no finding from the canonical physical rule set.
// Evaluating those rules here avoids a second, drift-prone definition of "clean
// physical layer" for the outlier cap. Reliable counters are not enough on their
// own: a side that exposes no receive-error counter reports an empty, reliable
// set by construction, which cannot establish that the layer was clean.
func physicalLayerClean(f *Facts, thresholds Thresholds) bool {
	if !f.PC1.DeltaOK || !f.PC2.DeltaOK {
		return false
	}
	if !f.PC1.RXErrorEvidence || !f.PC2.RXErrorEvidence {
		return false
	}
	for _, rule := range Rules() {
		if rule.Category == model.CategoryPhysical && rule.Evaluate(f, thresholds) != nil {
			return false
		}
	}
	return true
}
