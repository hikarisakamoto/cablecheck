package testsuite

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"cablecheck/internal/model"
	"cablecheck/internal/parser"
	"cablecheck/internal/runner"
)

// LinkInspector reads the negotiated link state of one interface via
// `ethtool <if>` and derives human-readable observations from it.
type LinkInspector struct {
	// R runs ethtool.
	R runner.Runner
	// IfName is the interface under test.
	IfName string
	// RawDir, when non-empty, receives the verbatim ethtool output.
	RawDir string
}

// Inspect runs `ethtool <if>` and returns the parsed link settings plus the
// observations of ObserveLink. A missing ethtool or a non-zero exit is an
// error: unlike counters there is no fallback source for link settings. The
// spec carries the queryToolTimeout backstop so a wedged ethtool ioctl
// cannot hang a session whose ctx has no deadline of its own.
func (l *LinkInspector) Inspect(ctx context.Context) (model.LinkSettings, []string, error) {
	res, err := l.R.Run(ctx, teeRaw(runner.CommandSpec{
		Name:    "ethtool",
		Args:    []string{l.IfName},
		Timeout: queryToolTimeout,
		Label:   "ethtool-settings",
	}, l.RawDir))
	if err != nil {
		return model.LinkSettings{}, nil, fmt.Errorf("testsuite: link settings: %w", err)
	}
	if res.ExitCode != 0 {
		return model.LinkSettings{}, nil, fmt.Errorf("testsuite: link settings: ethtool %s exited %d: %s",
			l.IfName, res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	settings := parser.ParseEthtoolSettings(res.Stdout)
	return settings, ObserveLink(settings), nil
}

// ObserveLink derives conservative, human-readable observations from parsed
// link settings: no link at all, a half-duplex negotiation, and a negotiated
// speed below what both ends advertise. It is pure so the evaluator and tests
// can call it on any settings value.
func ObserveLink(ls model.LinkSettings) []string {
	var obs []string

	if !ls.LinkDetected || ls.SpeedMbps <= 0 {
		obs = append(obs, "no link detected: ethtool reports the link as down "+
			"(cable unplugged, defective cable, or the peer interface is down)")
		return obs
	}

	if strings.EqualFold(ls.Duplex, "half") {
		obs = append(obs, fmt.Sprintf("link negotiated half duplex at %d Mb/s; "+
			"possible causes: a damaged cable pair, an autonegotiation fault, "+
			"or a peer forced to half duplex", ls.SpeedMbps))
	}

	mutual := mutualAdvertisedMbps(ls)
	if mutual > 0 && ls.SpeedMbps < mutual {
		obs = append(obs, fmt.Sprintf("link negotiated %d Mb/s although both ends "+
			"advertise %d Mb/s — reduced speed can indicate damaged pairs or a "+
			"poor connector", ls.SpeedMbps, mutual))
	}

	return obs
}

// mutualAdvertisedMbps returns the highest speed advertised by BOTH ends, or
// 0 when either side's advertisement is unknown (no partner report means the
// comparison would prove nothing).
func mutualAdvertisedMbps(ls model.LinkSettings) int {
	local := maxModeMbps(ls.AdvertisedModes)
	partner := maxModeMbps(ls.PartnerModes)
	if local <= 0 || partner <= 0 {
		return 0
	}
	return min(local, partner)
}

// maxModeMbps extracts the highest speed from link-mode names such as
// "1000baseT/Full" or "2500baseT/Full"; 0 when none parse.
func maxModeMbps(modes []string) int {
	best := 0
	for _, mode := range modes {
		i := strings.Index(strings.ToLower(mode), "base")
		if i <= 0 {
			continue
		}
		speed, err := strconv.Atoi(mode[:i])
		if err != nil {
			continue
		}
		if speed > best {
			best = speed
		}
	}
	return best
}
