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
