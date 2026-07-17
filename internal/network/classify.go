package network

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DefaultSysfsRoot is the real sysfs directory holding one entry per network
// interface.
const DefaultSysfsRoot = "/sys/class/net"

// ARPHRD link-layer type numbers from the sysfs "type" file (linux/if_arp.h).
const (
	arphrdEther    = 1
	arphrdLoopback = 772
	arphrdNone     = 65534
)

// virtualDevtypes are uevent DEVTYPE values that mark software-defined
// devices even when a device symlink exists.
var virtualDevtypes = map[string]bool{
	"bridge":    true,
	"vlan":      true,
	"bond":      true,
	"vxlan":     true,
	"wireguard": true,
	"geneve":    true,
	"macvlan":   true,
	"macsec":    true,
}

// virtualNamePrefixes is the belt-and-braces name heuristic, applied last:
// these prefixes match the common virtual-device naming conventions.
var virtualNamePrefixes = []string{
	"veth", "br-", "docker", "tun", "tap", "wg",
	"virbr", "vmnet", "vnet", "zt", "tailscale",
}

// Class is the physical/virtual classification of an interface, with the
// evidence that produced it.
type Class struct {
	// Loopback marks the loopback interface (allowed only with
	// --allow-virtual-interface; powers the single-machine demo).
	Loopback bool
	// Virtual marks software-defined devices (bridge, veth, wg, vlan, ...)
	// whose results say nothing about a physical cable.
	Virtual bool
	// Wireless marks 802.11 devices.
	Wireless bool
	// USB marks NICs attached over USB — recorded as host-limitation
	// evidence for the evaluator (r8152 dongles), not a rejection.
	USB bool
	// Driver is the kernel driver name from the device/driver symlink;
	// empty when the interface has no device entry.
	Driver string
	// Reason is human-readable evidence for the first classification rule
	// that fired; empty for a plain physical NIC.
	Reason string
}

// Classify classifies the named interface purely from sysfs. sysfsRoot
// overrides /sys/class/net (tests inject a t.TempDir tree); empty means
// DefaultSysfsRoot. The link-layer type is taken from the sysfs "type" file;
// Discover, which also has `ip -j` metadata, feeds the richer link_type
// through the same ordered rules.
func Classify(sysfsRoot, name string) Class {
	root := sysfsRoot
	if root == "" {
		root = DefaultSysfsRoot
	}
	linkType, loopback := sysfsLinkType(root, name)
	return classify(root, name, linkType, loopback)
}

// classify applies the ordered classification rules (first hit wins the
// Reason): loopback → link type not ethernet → wireless → missing device
// symlink → uevent DEVTYPE → name-prefix heuristics. linkType uses the
// `ip -j` spelling ("ether", "loopback", "none", ...); empty means unknown,
// which skips the first two rules. Driver and USB are populated regardless
// of which rule fires.
func classify(root, name, linkType string, loopback bool) Class {
	cls := Class{
		Driver: readDriver(root, name),
		USB:    deviceIsUSB(root, name),
	}
	devtype := ueventDevtype(root, name)
	switch {
	case linkType == "loopback" || loopback:
		cls.Loopback = true
		cls.Reason = "loopback interface"
	case linkType != "" && linkType != "ether":
		cls.Virtual = true
		cls.Reason = fmt.Sprintf("link type %q is not ethernet", linkType)
	case hasWirelessDir(root, name) || devtype == "wlan":
		cls.Wireless = true
		cls.Reason = "wireless (802.11) interface"
	case !hasDeviceEntry(root, name):
		cls.Virtual = true
		cls.Reason = "no device entry in sysfs (not backed by hardware)"
	case virtualDevtypes[devtype]:
		cls.Virtual = true
		cls.Reason = "sysfs uevent DEVTYPE=" + devtype
	default:
		for _, prefix := range virtualNamePrefixes {
			if strings.HasPrefix(name, prefix) {
				cls.Virtual = true
				cls.Reason = fmt.Sprintf("interface name matches virtual-device pattern %q", prefix)
				break
			}
		}
	}
	return cls
}

// sysfsLinkType maps the sysfs ARPHRD "type" file to the `ip -j` link_type
// vocabulary. An unreadable or unparseable file yields ("", false): unknown,
// so the link-type rules are skipped rather than guessed.
func sysfsLinkType(root, name string) (linkType string, loopback bool) {
	data, err := os.ReadFile(filepath.Join(root, name, "type"))
	if err != nil {
		return "", false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return "", false
	}
	switch n {
	case arphrdEther:
		return "ether", false
	case arphrdLoopback:
		return "loopback", true
	case arphrdNone:
		return "none", false
	default:
		return fmt.Sprintf("arphrd %d", n), false
	}
}

// readDriver returns the driver name from the device/driver symlink, or ""
// when unreadable (no device entry, or a driverless pseudo-device).
func readDriver(root, name string) string {
	target, err := os.Readlink(filepath.Join(root, name, "device", "driver"))
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}

// deviceIsUSB reports whether the resolved device path crosses a USB bus
// segment — evidence that throughput may be host-limited by the dongle.
func deviceIsUSB(root, name string) bool {
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, name, "device"))
	if err != nil {
		return false
	}
	return strings.Contains(resolved, "/usb")
}

// hasDeviceEntry reports whether the interface has a sysfs device symlink;
// its absence marks software-only devices (veth, bridge, bond, vlan, wg).
func hasDeviceEntry(root, name string) bool {
	_, err := os.Lstat(filepath.Join(root, name, "device"))
	return err == nil
}

// hasWirelessDir reports whether the sysfs wireless subdirectory exists,
// marking 802.11 devices even though they have a real device symlink.
func hasWirelessDir(root, name string) bool {
	info, err := os.Stat(filepath.Join(root, name, "wireless"))
	return err == nil && info.IsDir()
}

// ueventDevtype extracts the DEVTYPE value from the interface's uevent file;
// "" when the file or the key is absent.
func ueventDevtype(root, name string) string {
	data, err := os.ReadFile(filepath.Join(root, name, "uevent"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "DEVTYPE="); ok {
			return v
		}
	}
	return ""
}
