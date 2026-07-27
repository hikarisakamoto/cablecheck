package reporting

import (
	"math"
	"slices"

	"cablecheck/internal/model"
)

// tcpTrialSpread derives inter-trial throughput statistics from the raw TCP
// results. Raw trials remain authoritative: renderers recompute this aggregate
// so saved or externally produced derived fields cannot become stale.
func tcpTrialSpread(results []model.TCPResult) *model.TCPTrialSpread {
	spread := &model.TCPTrialSpread{
		PC1ToPC2: tcpTrialStats(results, model.DirectionPC1ToPC2),
		PC2ToPC1: tcpTrialStats(results, model.DirectionPC2ToPC1),
	}
	if spread.PC1ToPC2 == nil && spread.PC2ToPC1 == nil {
		return nil
	}
	return spread
}

// tcpTrialStats returns nil until a direction has at least two completed
// trials. Receiver-observed throughput is preferred; sender throughput is the
// fallback when the receiver summary is non-positive, matching evaluation.
func tcpTrialStats(results []model.TCPResult, direction string) *model.TCPTrialStats {
	bitrates := make([]float64, 0, len(results))
	for _, result := range results {
		if result.Direction != direction || result.Incomplete {
			continue
		}
		bitrate := result.ReceiverBitsPerSecond
		if bitrate <= 0 {
			bitrate = result.SenderBitsPerSecond
		}
		bitrates = append(bitrates, bitrate)
	}
	if len(bitrates) < 2 {
		return nil
	}

	slices.Sort(bitrates)
	return &model.TCPTrialStats{
		CompletedTrials:        len(bitrates),
		MinimumBitsPerSecond:   bitrates[0],
		MedianBitsPerSecond:    bitrates[(len(bitrates)-1)/2],
		MaximumBitsPerSecond:   bitrates[len(bitrates)-1],
		CoefficientOfVariation: populationCoV(bitrates),
	}
}

// populationCoV computes population standard deviation divided by the mean.
// Scaling by the largest value keeps ordinary finite bitrate samples away from
// overflow without changing the ratio. A zero mean has no relative variation.
func populationCoV(sortedValues []float64) float64 {
	maxValue := sortedValues[len(sortedValues)-1]
	if maxValue == 0 {
		return 0
	}

	mean := 0.0
	for _, value := range sortedValues {
		mean += value / maxValue
	}
	mean /= float64(len(sortedValues))
	if mean == 0 {
		return 0
	}

	variance := 0.0
	for _, value := range sortedValues {
		delta := value/maxValue - mean
		variance += delta * delta
	}
	return math.Sqrt(variance/float64(len(sortedValues))) / math.Abs(mean)
}
