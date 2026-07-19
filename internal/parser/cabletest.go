package parser

import (
	"bufio"
	"bytes"
	"regexp"
	"strconv"
	"strings"

	"cablecheck/internal/model"
)

var (
	cablePairCodeRE = regexp.MustCompile(`^Pair ([A-D]) code (.+)$`)
	cableFaultRE    = regexp.MustCompile(`^Pair ([A-D]), fault length: ([0-9.]+)m$`)
	tdrPairRE       = regexp.MustCompile(`Pair ([A-D])`)
	tdrDistanceRE   = regexp.MustCompile(`(?i)distance[:,\s]+([0-9.]+)\s*(cm|m)\b`)
	tdrAmplitudeRE  = regexp.MustCompile(`(?i)amplitude[:,\s]+(-?[0-9]+)\b`)
)

// ParseCableTest parses netlink-era `ethtool --cable-test` output. Known
// command failures return an unavailable result with no pairs, so lack of
// driver support, privilege, or ethtool support can never become a cable
// fault.
func ParseCableTest(stdout, stderr []byte, exitCode int) model.CableTestResult {
	if exitCode != 0 || oldEthtoolCableOutput(stdout, stderr) {
		return unavailableCableTest(stdout, stderr)
	}

	result := model.CableTestResult{Available: true}
	byPair := make(map[string]int, 4)
	scanner := bufio.NewScanner(bytes.NewReader(stdout))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if match := cablePairCodeRE.FindStringSubmatch(line); match != nil {
			pair := model.CablePairResult{
				Pair:     match[1],
				Status:   cablePairStatus(match[2]),
				RawCode:  match[2],
				HasFault: match[2] != "OK",
			}
			byPair[pair.Pair] = len(result.Pairs)
			result.Pairs = append(result.Pairs, pair)
			continue
		}
		if match := cableFaultRE.FindStringSubmatch(line); match != nil {
			distance, err := strconv.ParseFloat(match[2], 64)
			if err != nil {
				continue
			}
			if index, ok := byPair[match[1]]; ok {
				result.Pairs[index].FaultMeters = distance
				result.Pairs[index].HasFault = true
			}
		}
	}
	return result
}

// ParseCableTestTDR parses `ethtool --cable-test-tdr` output tolerantly.
// Within each pair line it extracts pair, distance and signed amplitude
// independently; format drift merely increments UnparsedLines.
func ParseCableTestTDR(stdout, stderr []byte, exitCode int) model.CableTestResult {
	if exitCode != 0 || oldEthtoolCableOutput(stdout, stderr) {
		return unavailableCableTest(stdout, stderr)
	}
	result := model.CableTestResult{Available: true}
	scanner := bufio.NewScanner(bytes.NewReader(stdout))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "Cable test TDR data for device ") {
			continue
		}
		pair := tdrPairRE.FindStringSubmatch(line)
		distance := tdrDistanceRE.FindStringSubmatch(line)
		amplitude := tdrAmplitudeRE.FindStringSubmatch(line)
		if pair == nil || distance == nil || amplitude == nil {
			result.UnparsedLines++
			continue
		}
		distanceM, distanceErr := strconv.ParseFloat(distance[1], 64)
		amp, amplitudeErr := strconv.Atoi(amplitude[1])
		if distanceErr != nil || amplitudeErr != nil {
			result.UnparsedLines++
			continue
		}
		if strings.EqualFold(distance[2], "cm") {
			distanceM /= 100
		}
		result.Samples = append(result.Samples, model.TDRSample{
			Pair: pair[1], DistanceM: distanceM, Amplitude: amp,
		})
	}
	return result
}

func oldEthtoolCableOutput(stdout, stderr []byte) bool {
	combined := string(append(append([]byte(nil), stderr...), stdout...))
	return strings.Contains(combined, "bad command line argument") ||
		strings.Contains(combined, "Usage: ethtool")
}

func cablePairStatus(code string) model.PairStatus {
	switch code {
	case "OK":
		return model.PairOK
	case "Open Circuit":
		return model.PairOpen
	case "Short within Pair":
		return model.PairShortIntra
	case "Short to another pair":
		return model.PairShortInter
	case "Impedance mismatch":
		return model.PairImpedance
	default:
		return model.PairUnspecified
	}
}

func unavailableCableTest(stdout, stderr []byte) model.CableTestResult {
	combined := string(append(append([]byte(nil), stderr...), stdout...))
	reason := "cable test failed"
	switch {
	case strings.Contains(combined, "Operation not supported"):
		reason = "driver does not support cable test"
	case strings.Contains(combined, "Operation not permitted"):
		reason = "requires root"
	case strings.Contains(combined, "bad command line argument") ||
		strings.Contains(combined, "Usage: ethtool"):
		reason = "ethtool too old"
	}
	return model.CableTestResult{Available: false, UnavailableReason: reason}
}
