// Package parser turns the raw output of the external tools CableCheck runs
// (ethtool, ip -j, ping, iperf3) into typed data.
//
// Every parser here is pure: bytes in, structs out, no exec, no clock, no
// I/O. Lines that a text parser does not recognize are counted, never fatal —
// the verbatim tool output is always preserved elsewhere for forensics.
package parser

import (
	"bufio"
	"bytes"
	"regexp"
	"strconv"
	"strings"

	"cablecheck/internal/model"
)

// speedRe matches the numeric form of an ethtool "Speed:" value ("1000Mb/s").
var speedRe = regexp.MustCompile(`^(\d+)Mb/s$`)

// linkModeListKeys are the ethtool settings keys whose values are link-mode
// lists that may continue on subsequent deeper-indented, colon-free lines.
var linkModeListKeys = map[string]bool{
	"Supported link modes":               true,
	"Advertised link modes":              true,
	"Link partner advertised link modes": true,
}

// ParseEthtoolSettings parses the output of `ethtool <iface>` into
// model.LinkSettings.
//
// It is a line-oriented state machine: a line containing a colon starts a
// key/value pair; the three link-mode keys enter list mode, in which
// subsequent colon-free lines are whitespace-split into mode tokens
// ("1000baseT/Full"). A value of "Not reported" yields an empty list.
// "Speed: Unknown!" maps to SpeedMbps == -1 (never 0). Colon-free
// continuation lines outside list mode (e.g. the second line of "Current
// message level") are appended to the previous key's Raw entry.
func ParseEthtoolSettings(out []byte) model.LinkSettings {
	ls := model.LinkSettings{
		SpeedMbps: -1,
		Duplex:    "unknown",
		AutoNeg:   "unknown",
		Raw:       map[string]string{},
	}

	var listTarget *[]string // non-nil while in link-mode list mode
	lastKey := ""            // last simple key seen, for Raw continuations

	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), " \t\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		idx := strings.Index(trimmed, ":")
		if idx < 0 {
			// Colon-free line: link-mode tokens in list mode, otherwise a
			// continuation of the previous simple value.
			if listTarget != nil {
				*listTarget = append(*listTarget, strings.Fields(trimmed)...)
			} else if lastKey != "" {
				if prev := ls.Raw[lastKey]; prev != "" {
					ls.Raw[lastKey] = prev + " " + trimmed
				} else {
					ls.Raw[lastKey] = trimmed
				}
			}
			continue
		}

		key := strings.TrimSpace(trimmed[:idx])
		value := strings.TrimSpace(trimmed[idx+1:])
		listTarget = nil
		lastKey = ""

		if linkModeListKeys[key] {
			target := modeListField(&ls, key)
			if value != "Not reported" {
				*target = append(*target, strings.Fields(value)...)
				listTarget = target
			}
			continue
		}

		lastKey = key
		ls.Raw[key] = value

		switch key {
		case "Speed":
			if m := speedRe.FindStringSubmatch(value); m != nil {
				if v, err := strconv.Atoi(m[1]); err == nil {
					ls.SpeedMbps = v
				}
			} else {
				ls.SpeedMbps = -1 // "Unknown!" or anything unrecognized
			}
		case "Duplex":
			switch {
			case strings.HasPrefix(value, "Full"):
				ls.Duplex = "full"
			case strings.HasPrefix(value, "Half"):
				ls.Duplex = "half"
			default:
				ls.Duplex = "unknown" // "Unknown! (255)"
			}
		case "Auto-negotiation":
			switch value {
			case "on", "off":
				ls.AutoNeg = value
			default:
				ls.AutoNeg = "unknown"
			}
		case "Port":
			ls.Port = value
		case "MDI-X":
			ls.MDIX = value
		case "Link detected":
			ls.LinkDetected = value == "yes"
		case "Supported ports":
			// "[ TP    MII ]" -> ["TP", "MII"]
			v := strings.TrimSuffix(strings.TrimPrefix(value, "["), "]")
			ls.SupportedPorts = append(ls.SupportedPorts, strings.Fields(v)...)
		}
	}
	return ls
}

// modeListField returns the LinkSettings slice a link-mode list key feeds.
func modeListField(ls *model.LinkSettings, key string) *[]string {
	switch key {
	case "Supported link modes":
		return &ls.SupportedModes
	case "Advertised link modes":
		return &ls.AdvertisedModes
	default: // "Link partner advertised link modes"
		return &ls.PartnerModes
	}
}

// statLineRe matches one `ethtool -S` counter line: a name (letters, digits,
// and the separator characters drivers actually use) followed by a
// non-negative decimal value.
var statLineRe = regexp.MustCompile(`^\s*([A-Za-z0-9_\[\]. -]+?):\s+(\d+)\s*$`)

// ParseEthtoolStats parses `ethtool -S <iface>` output into a counter map.
//
// The banner line ("NIC statistics:") and anything that is not a
// "name: <decimal>" line (section headers, hex dumps) are ignored — absent
// keys mean "no data", which is not the same as zero. If a driver ever emits
// a duplicate counter name, the last occurrence wins.
func ParseEthtoolStats(out []byte) map[string]uint64 {
	stats := map[string]uint64{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		m := statLineRe.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		v, err := strconv.ParseUint(m[2], 10, 64)
		if err != nil {
			continue // value overflow; treat as junk
		}
		stats[strings.TrimSpace(m[1])] = v
	}
	return stats
}
