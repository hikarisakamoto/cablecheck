package parser

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"cablecheck/internal/model"
)

// ErrNoStats64 reports `ip -j -s -s link show` output whose element lacks the
// stats64 object; all supported kernels emit it, so its absence means the
// command was run without statistics flags or against something exotic.
var ErrNoStats64 = errors.New("ip link output has no stats64 object")

// IPAddrInfo is one entry of an interface's addr_info array in
// `ip -j addr show` output. Field names match iproute2's JSON exactly.
type IPAddrInfo struct {
	// Family is "inet" or "inet6".
	Family string `json:"family"`
	// Local is the assigned address.
	Local string `json:"local"`
	// Prefixlen is the prefix length of the assigned address.
	Prefixlen int `json:"prefixlen"`
	// Scope is "global", "host" or "link".
	Scope string `json:"scope"`
	// Label is the address label (usually the interface name).
	Label string `json:"label"`
	// Dynamic is true for addresses installed by DHCP or SLAAC.
	Dynamic bool `json:"dynamic"`
}

// IPLink is one element of `ip -j addr show` / `ip -j -s -s link show`
// output. Field names match iproute2's JSON exactly (verified live);
// Operstate is normalized to lowercase at parse time so it compares directly
// against sysfs values.
type IPLink struct {
	// Ifindex is the kernel interface index.
	Ifindex int `json:"ifindex"`
	// Ifname is the interface name.
	Ifname string `json:"ifname"`
	// Flags holds the uppercase link flags ("BROADCAST", "UP", "LOWER_UP",
	// "NO-CARRIER", "LOOPBACK", "POINTOPOINT", "NOARP").
	Flags []string `json:"flags"`
	// MTU is the interface MTU.
	MTU int `json:"mtu"`
	// Operstate is the operational state, normalized to lowercase
	// ("up", "down", "unknown") — iproute2 emits it UPPERCASE, sysfs
	// lowercase, and CableCheck standardizes on the sysfs spelling.
	Operstate string `json:"operstate"`
	// LinkType is "ether", "loopback", or "none" (WireGuard/tun).
	LinkType string `json:"link_type"`
	// Address is the hardware (MAC) address; absent for link/none devices.
	Address string `json:"address"`
	// Altnames lists alternative interface names, when present.
	Altnames []string `json:"altnames"`
	// AddrInfo lists the assigned addresses (addr show only).
	AddrInfo []IPAddrInfo `json:"addr_info"`
	// Stats64 holds the interface counters (`-s -s` link show only).
	Stats64 *model.IPStats64 `json:"stats64"`
}

// ParseIPAddr parses the JSON array printed by `ip -j addr show` into IPLink
// values. Operstate is normalized to lowercase; unknown JSON fields are
// ignored.
func ParseIPAddr(out []byte) ([]IPLink, error) {
	var links []IPLink
	if err := json.Unmarshal(out, &links); err != nil {
		return nil, fmt.Errorf("ip addr JSON: %w", err)
	}
	for i := range links {
		links[i].Operstate = strings.ToLower(links[i].Operstate)
	}
	return links, nil
}

// ParseIPLinkStats parses the JSON array printed by
// `ip -j -s -s link show dev <iface>`.
//
// Because the command names a single device, the array must contain exactly
// one element, and that element must carry a stats64 object (ErrNoStats64
// otherwise). The detailed error counters are flat fields inside stats64.rx
// and stats64.tx (rx.crc_errors, tx.carrier_changes, ...) — see
// model.IPStats64.
func ParseIPLinkStats(out []byte) (IPLink, error) {
	var links []IPLink
	if err := json.Unmarshal(out, &links); err != nil {
		return IPLink{}, fmt.Errorf("ip link JSON: %w", err)
	}
	if len(links) != 1 {
		return IPLink{}, fmt.Errorf("ip link show dev output has %d elements, want exactly 1", len(links))
	}
	link := links[0]
	link.Operstate = strings.ToLower(link.Operstate)
	if link.Stats64 == nil {
		return IPLink{}, fmt.Errorf("interface %s: %w", link.Ifname, ErrNoStats64)
	}
	return link, nil
}
