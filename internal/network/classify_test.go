package network

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClassifyOrderedRules(t *testing.T) {
	tests := []struct {
		name     string
		iface    string
		linkType string
		loopback bool
		setup    func(*testing.T, string, string)
		want     Class
	}{
		{
			name:     "physical ethernet",
			iface:    "eno1",
			linkType: "ether",
			setup: func(t *testing.T, root, name string) {
				setDevice(t, root, name, "pci0000:00/0000:00:1f.6", "e1000e")
			},
			want: Class{Driver: "e1000e"},
		},
		{
			name:     "loopback wins and retains device evidence",
			iface:    "docker0",
			linkType: "ether",
			loopback: true,
			setup: func(t *testing.T, root, name string) {
				setDevice(t, root, name, "pci0000:00/usb1/1-2/1-2:1.0", "r8152")
				setWireless(t, root, name)
				setUevent(t, root, name, "DEVTYPE=bridge\n")
			},
			want: Class{Loopback: true, USB: true, Driver: "r8152", Reason: "loopback interface"},
		},
		{
			name:     "non ethernet wins over wireless",
			iface:    "wg0",
			linkType: "none",
			setup: func(t *testing.T, root, name string) {
				setDevice(t, root, name, "virtual/pseudo", "")
				setWireless(t, root, name)
			},
			want: Class{Virtual: true, Reason: `link type "none" is not ethernet`},
		},
		{
			name:     "wireless directory",
			iface:    "wlan0",
			linkType: "ether",
			setup: func(t *testing.T, root, name string) {
				setDevice(t, root, name, "pci0000:00/0000:00:14.3", "iwlwifi")
				setWireless(t, root, name)
			},
			want: Class{Wireless: true, Driver: "iwlwifi", Reason: "wireless (802.11) interface"},
		},
		{
			name:     "wireless devtype wins over missing device",
			iface:    "wlp2s0",
			linkType: "ether",
			setup: func(t *testing.T, root, name string) {
				setUevent(t, root, name, "INTERFACE=wlp2s0\nDEVTYPE=wlan\n")
			},
			want: Class{Wireless: true, Reason: "wireless (802.11) interface"},
		},
		{
			name:     "missing device wins over virtual metadata",
			iface:    "docker0",
			linkType: "ether",
			setup: func(t *testing.T, root, name string) {
				setUevent(t, root, name, "DEVTYPE=bridge\n")
			},
			want: Class{Virtual: true, Reason: "no device entry in sysfs (not backed by hardware)"},
		},
		{
			name:     "unknown metadata remains physical",
			iface:    "ethveth0",
			linkType: "ether",
			setup: func(t *testing.T, root, name string) {
				setDevice(t, root, name, "pci0000:00/0000:02:00.0", "igc")
				setUevent(t, root, name, "DEVTYPE=physical\n")
			},
			want: Class{Driver: "igc"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := newSysfs(t)
			tc.setup(t, root, tc.iface)
			if got := classify(root, tc.iface, tc.linkType, tc.loopback); got != tc.want {
				t.Errorf("classify() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestClassifyVirtualDevtypes(t *testing.T) {
	for _, devtype := range []string{
		"bridge", "vlan", "bond", "vxlan", "wireguard", "geneve", "macvlan", "macsec",
	} {
		t.Run(devtype, func(t *testing.T) {
			root := newSysfs(t)
			const name = "eno1.100"
			setDevice(t, root, name, "virtual/pseudo", "")
			setUevent(t, root, name, "DEVTYPE="+devtype+"\n")

			want := Class{Virtual: true, Reason: "sysfs uevent DEVTYPE=" + devtype}
			if got := classify(root, name, "ether", false); got != want {
				t.Errorf("classify() = %+v, want %+v", got, want)
			}
		})
	}
}

func TestClassifyVirtualNamePrefixes(t *testing.T) {
	tests := []struct {
		name   string
		prefix string
	}{
		{name: "veth0", prefix: "veth"},
		{name: "br-test", prefix: "br-"},
		{name: "docker0", prefix: "docker"},
		{name: "tun0", prefix: "tun"},
		{name: "tap0", prefix: "tap"},
		{name: "wg0", prefix: "wg"},
		{name: "virbr0", prefix: "virbr"},
		{name: "vmnet0", prefix: "vmnet"},
		{name: "vnet0", prefix: "vnet"},
		{name: "zt0", prefix: "zt"},
		{name: "tailscale0", prefix: "tailscale"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := newSysfs(t)
			setDevice(t, root, tc.name, "virtual/pseudo", "")
			setUevent(t, root, tc.name, "INTERFACE="+tc.name+"\n")

			want := Class{Virtual: true, Reason: `interface name matches virtual-device pattern "` + tc.prefix + `"`}
			if got := classify(root, tc.name, "ether", false); got != want {
				t.Errorf("classify() = %+v, want %+v", got, want)
			}
		})
	}
}

func TestClassifySysfsLinkType(t *testing.T) {
	tests := []struct {
		name     string
		typeData *string
		want     Class
	}{
		{name: "ethernet", typeData: stringPtr(" 1 \n"), want: Class{Driver: "e1000e"}},
		{name: "loopback", typeData: stringPtr("772\n"), want: Class{Loopback: true, Driver: "e1000e", Reason: "loopback interface"}},
		{name: "none", typeData: stringPtr("65534\n"), want: Class{Virtual: true, Driver: "e1000e", Reason: `link type "none" is not ethernet`}},
		{name: "unknown", typeData: stringPtr("42\n"), want: Class{Virtual: true, Driver: "e1000e", Reason: `link type "arphrd 42" is not ethernet`}},
		{name: "malformed", typeData: stringPtr("not-a-number\n"), want: Class{Driver: "e1000e"}},
		{name: "missing", want: Class{Driver: "e1000e"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := newSysfs(t)
			const name = "eno1"
			setDevice(t, root, name, "pci0000:00/0000:00:1f.6", "e1000e")
			if tc.typeData != nil {
				if err := os.WriteFile(filepath.Join(root, name, "type"), []byte(*tc.typeData), 0o644); err != nil {
					t.Fatalf("write type: %v", err)
				}
			}

			if got := Classify(root, name); got != tc.want {
				t.Errorf("Classify() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestClassifyPhysicalUSBDevice(t *testing.T) {
	root := newSysfs(t)
	const name = "enp0s20u1"
	setType(t, root, name, arphrdEther)
	setDevice(t, root, name, "pci0000:00/0000:00:14.0/usb1/1-2/1-2:1.0", "r8152")

	want := Class{USB: true, Driver: "r8152"}
	if got := Classify(root, name); got != want {
		t.Errorf("Classify() = %+v, want %+v", got, want)
	}
}

func stringPtr(value string) *string {
	return &value
}
