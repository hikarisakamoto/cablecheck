package parser

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"cablecheck/internal/model"
)

// ErrIntervalRejected reports that ping refused the requested -i interval
// (exit code 2 plus the "cannot flood" / "minimal interval" stderr message,
// whose exact wording differs across iputils generations). The caller retries
// with the next rung of the interval ladder.
var ErrIntervalRejected = errors.New("ping rejected the requested interval")

// ErrUnsupportedPingFormat reports output in a shape CableCheck does not
// support (busybox ping). It exists so busybox output is surfaced as a
// tooling problem instead of being silently misread as 100% packet loss.
var ErrUnsupportedPingFormat = errors.New("unsupported ping output format (busybox ping is not supported; install iputils)")

// Line grammar for iputils ping under LC_ALL=C (verified shapes).
var (
	pingReplyRe    = regexp.MustCompile(`^\[(\d+\.\d+)\] (\d+) bytes from ([0-9a-fA-F:.]+): icmp_seq=(\d+) ttl=(\d+) time=([\d.]+) ms( \(DUP!\))?$`)
	pingIcmpErrRe  = regexp.MustCompile(`^\[?[\d.]*\]? ?From (\S+):? icmp_seq=(\d+) (.+)$`)
	pingLocalErrRe = regexp.MustCompile(`^(\[[\d.]+\] )?ping: (local error: .+|sendmsg: .+)$`)
	pingSummaryRe  = regexp.MustCompile(`^(\d+) packets transmitted, (\d+) received(?:, \+(\d+) duplicates)?(?:, \+(\d+) errors)?, ([\d.]+)% packet loss, time (\d+)ms$`)
	pingRTTRe      = regexp.MustCompile(`^rtt min/avg/max/mdev = ([\d.]+)/([\d.]+)/([\d.]+)/([\d.]+) ms(, pipe \d+)?$`)
	pingHeaderRe   = regexp.MustCompile(`^PING (\S+) \(([0-9a-fA-F:.]+)\) \d+\(\d+\) bytes of data\.$`)
	pingFooterRe   = regexp.MustCompile(`^--- (\S+) ping statistics ---$`)
	// Busybox shapes, recognized only far enough to reject them.
	busyboxReplyRe = regexp.MustCompile(`^\d+ bytes from \S+: seq=\d+ `)
	busyboxRTTRe   = regexp.MustCompile(`^round-trip min/avg/max = `)
)

// ParsePing parses per-packet iputils ping output (run with -n -D under
// LC_ALL=C) plus its stderr and exit code into a model.PingResult.
//
// Semantics:
//   - Exit code 1 with a parsed summary is a valid result carrying loss, not
//     an error; only exit 2 is an invocation problem.
//   - Exit 2 with stderr containing both "cannot flood" and "minimal
//     interval" (the substrings shared by every iputils wording) returns
//     ErrIntervalRejected so the caller can climb the interval ladder.
//   - RTT percentiles (nearest-rank, 50/90/95/99) and spike detection use
//     only the FIRST reply per icmp_seq; DUP replies are counted separately
//     in Duplicates — on a direct cable they are evidence of their own.
//   - MissingSeqRuns localizes which sequence numbers got no echo reply;
//     LongestGapMs is the largest delta between consecutive -D reply
//     timestamps (captures burst loss and stalls).
//   - Busybox-shaped output returns ErrUnsupportedPingFormat instead of a
//     bogus 100%-loss result.
//   - Unrecognized stdout lines are counted in UnparsedLines, never fatal.
func ParsePing(stdout, stderr []byte, exitCode int) (model.PingResult, error) {
	if exitCode == 2 {
		if bytes.Contains(stderr, []byte("cannot flood")) && bytes.Contains(stderr, []byte("minimal interval")) {
			return model.PingResult{ExitCode: exitCode},
				fmt.Errorf("%w: %s", ErrIntervalRejected, firstLine(stderr))
		}
		return model.PingResult{ExitCode: exitCode},
			fmt.Errorf("ping invocation failed (exit 2): %s", firstLine(stderr))
	}

	res := model.PingResult{ExitCode: exitCode}
	firstRTT := map[int]float64{} // icmp_seq -> RTT of the first reply
	var replyTimes []float64      // all reply timestamps, in output order
	summaryFound := false
	summaryDups := 0
	busybox := false

	sc := bufio.NewScanner(bytes.NewReader(stdout))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		switch {
		case pingReplyRe.MatchString(line):
			m := pingReplyRe.FindStringSubmatch(line)
			ts, _ := strconv.ParseFloat(m[1], 64)
			seq, _ := strconv.Atoi(m[4])
			rtt, _ := strconv.ParseFloat(m[6], 64)
			replyTimes = append(replyTimes, ts)
			if _, seen := firstRTT[seq]; m[7] != "" || seen {
				res.Duplicates++
			} else {
				firstRTT[seq] = rtt
			}
		case pingHeaderRe.MatchString(line):
			res.Target = pingHeaderRe.FindStringSubmatch(line)[1]
		case pingSummaryRe.MatchString(line):
			m := pingSummaryRe.FindStringSubmatch(line)
			summaryFound = true
			res.Transmitted, _ = strconv.Atoi(m[1])
			res.Received, _ = strconv.Atoi(m[2])
			if m[3] != "" {
				summaryDups, _ = strconv.Atoi(m[3])
			}
			res.LossPercent, _ = strconv.ParseFloat(m[5], 64)
		case pingRTTRe.MatchString(line):
			m := pingRTTRe.FindStringSubmatch(line)
			res.RTTMinMs, _ = strconv.ParseFloat(m[1], 64)
			res.RTTAvgMs, _ = strconv.ParseFloat(m[2], 64)
			res.RTTMaxMs, _ = strconv.ParseFloat(m[3], 64)
			res.RTTMdevMs, _ = strconv.ParseFloat(m[4], 64)
		case pingIcmpErrRe.MatchString(line):
			res.IcmpErrors++
		case pingLocalErrRe.MatchString(line):
			res.SendErrors++
		case pingFooterRe.MatchString(line):
			if res.Target == "" {
				res.Target = pingFooterRe.FindStringSubmatch(line)[1]
			}
		case busyboxReplyRe.MatchString(line), busyboxRTTRe.MatchString(line):
			busybox = true
		default:
			res.UnparsedLines++
		}
	}

	if busybox && len(firstRTT) == 0 && !summaryFound {
		return model.PingResult{ExitCode: exitCode},
			fmt.Errorf("%w: busybox-shaped lines detected", ErrUnsupportedPingFormat)
	}
	if !summaryFound && len(firstRTT) == 0 {
		return model.PingResult{ExitCode: exitCode},
			errors.New("ping output not recognized: no replies and no summary line")
	}
	if summaryDups > res.Duplicates {
		res.Duplicates = summaryDups // reply lines were truncated; trust the summary
	}

	// Percentiles and spikes over the first reply per sequence number.
	if len(firstRTT) > 0 {
		rtts := make([]float64, 0, len(firstRTT))
		seqs := make([]int, 0, len(firstRTT))
		for seq, rtt := range firstRTT {
			rtts = append(rtts, rtt)
			seqs = append(seqs, seq)
		}
		slices.Sort(rtts)
		slices.Sort(seqs)
		res.Percentiles = map[int]float64{}
		for _, p := range []int{50, 90, 95, 99} {
			res.Percentiles[p] = nearestRank(rtts, p)
		}
		threshold := math.Max(5*res.Percentiles[50], res.Percentiles[50]+10.0)
		for _, seq := range seqs {
			if rtt := firstRTT[seq]; rtt > threshold {
				res.Spikes = append(res.Spikes, model.PingSpike{Seq: seq, RTTMs: rtt})
			}
		}
	}

	// Runs of sequence numbers that got no echo reply (iputils seq starts
	// at 1); only meaningful when the summary told us how many were sent.
	run := model.SeqRun{}
	flush := func() {
		if run.Len > 0 {
			res.MissingSeqRuns = append(res.MissingSeqRuns, run)
			res.LongestSeqGap = max(res.LongestSeqGap, run.Len)
			run = model.SeqRun{}
		}
	}
	for seq := 1; seq <= res.Transmitted; seq++ {
		if _, ok := firstRTT[seq]; ok {
			flush()
			continue
		}
		if run.Len == 0 {
			run.FirstSeq = seq
		}
		run.Len++
	}
	flush()

	// Longest silence between consecutive replies, from -D timestamps.
	for i := 1; i < len(replyTimes); i++ {
		gap := (replyTimes[i] - replyTimes[i-1]) * 1000
		if gap > res.LongestGapMs {
			res.LongestGapMs = gap
		}
	}

	return res, nil
}

// nearestRank returns the p-th percentile of sorted (ascending) values using
// the nearest-rank method: the value at index ceil(p/100*n)-1.
func nearestRank(sorted []float64, p int) float64 {
	rank := (p*len(sorted) + 99) / 100
	if rank < 1 {
		rank = 1
	}
	return sorted[rank-1]
}

// firstLine returns the first non-empty line of b, trimmed, for error text.
func firstLine(b []byte) string {
	for _, l := range strings.Split(string(b), "\n") {
		if t := strings.TrimSpace(l); t != "" {
			return t
		}
	}
	return ""
}
