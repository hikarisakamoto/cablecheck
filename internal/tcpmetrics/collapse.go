// Package tcpmetrics provides pure, shared analysis of normalized TCP
// interval measurements. It owns measurement definitions used before health
// evaluation so parsers and compatibility paths cannot diverge.
package tcpmetrics

import (
	"math"
	"slices"

	"cablecheck/internal/model"
)

// CollapseBelowMedianRatio is the canonical collapse boundary. An interval
// collapses only when its throughput is strictly below this share of the
// post-first-interval median.
const CollapseBelowMedianRatio = 0.5

// Sample is the subset of one iperf3 interval needed for collapse analysis.
type Sample struct {
	StartSec      float64
	BitsPerSecond float64
}

// CollapseEvents groups consecutive collapsed intervals. The first interval
// is excluded from both the median and detection whenever at least two rows
// exist, avoiding TCP slow-start as evidence. The returned slice is always
// non-nil to distinguish completed clean analysis from unavailable evidence.
func CollapseEvents(rows []Sample) []model.TCPCollapseEvent {
	events := make([]model.TCPCollapseEvent, 0)
	set := rows
	if len(rows) >= 2 {
		set = rows[1:]
	}
	if len(set) == 0 {
		return events
	}

	rates := make([]float64, len(set))
	for i, row := range set {
		if row.BitsPerSecond < 0 || math.IsNaN(row.BitsPerSecond) || math.IsInf(row.BitsPerSecond, 0) {
			return events
		}
		rates[i] = row.BitsPerSecond
	}
	slices.Sort(rates)
	median := rates[len(rates)/2]
	if len(rates)%2 == 0 {
		lower := rates[len(rates)/2-1]
		median = lower + (median-lower)/2
	}
	if median <= 0 {
		return events
	}

	threshold := CollapseBelowMedianRatio * median
	active := -1
	for _, row := range set {
		if row.BitsPerSecond >= threshold {
			active = -1
			continue
		}
		if active < 0 {
			events = append(events, model.TCPCollapseEvent{
				StartSec: row.StartSec,
				MinBps:   row.BitsPerSecond,
			})
			active = len(events) - 1
		}
		events[active].Len++
		events[active].MinBps = min(events[active].MinBps, row.BitsPerSecond)
	}
	return events
}
