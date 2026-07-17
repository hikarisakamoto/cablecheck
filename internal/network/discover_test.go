package network

import (
	"errors"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	"cablecheck/internal/runner"
	"cablecheck/internal/runner/runnertest"
)

// scriptIPAddr scripts `ip -j addr show` to return the named testdata/ip
// fixture.
func scriptIPAddr(fr *runnertest.FakeRunner, fixture string) {
	fr.Script(runnertest.Script{
		Name:       "ip",
		Match:      runnertest.ArgsExact("-j", "addr", "show"),
		StdoutFile: filepath.Join("..", "..", "testdata", "ip", fixture),
	})
}

func TestDiscoverByIP(t *testing.T) {
	fr := runnertest.New(t)
	scriptIPAddr(fr, "addr_multi.json")
	root := newSysfs(t)
	setDevice(t, root, "enp3s0", "pci0000:00/0000:00:1f.6", "e1000e")

	got, err := Discover(t.Context(), fr, netip.MustParseAddr("10.0.0.1"), DiscoverOptions{SysfsRoot: root})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	want := Iface{
		Name:      "enp3s0",
		Index:     2,
		MTU:       1500,
		Operstate: "up",
		MAC:       "9c:6b:00:aa:bb:01",
		PrefixLen: 24,
		Class:     Class{Driver: "e1000e"},
	}
	if got != want {
		t.Errorf("Discover = %+v, want %+v", got, want)
	}
}

func TestIPNotOnMachine(t *testing.T) {
	fr := runnertest.New(t)
	scriptIPAddr(fr, "addr_ip_not_found.json")

	_, err := Discover(t.Context(), fr, netip.MustParseAddr("10.0.0.7"), DiscoverOptions{SysfsRoot: newSysfs(t)})
	if !errors.Is(err, ErrIPNotAssigned) {
		t.Fatalf("err = %v, want ErrIPNotAssigned", err)
	}
	for _, want := range []string{"10.0.0.7", "did you mean", "enp9s0", "10.0.0.5/24"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing same-subnet hint fragment %q", err, want)
		}
	}
}

func TestLoopbackRejectedWithoutFlag(t *testing.T) {
	fr := runnertest.New(t)
	scriptIPAddr(fr, "addr_multi.json")

	_, err := Discover(t.Context(), fr, netip.MustParseAddr("127.0.0.1"), DiscoverOptions{SysfsRoot: newSysfs(t)})
	if !errors.Is(err, ErrNotPhysical) {
		t.Fatalf("err = %v, want ErrNotPhysical", err)
	}
	for _, want := range []string{"loopback", "--allow-virtual-interface"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestLoopbackAllowedWithFlag(t *testing.T) {
	tests := []struct {
		name string
		ip   string // 127.0.0.2 is only prefix-contained in 127.0.0.1/8 —
		// the single-machine demo depends on that match being accepted
	}{
		{name: "exact address", ip: "127.0.0.1"},
		{name: "prefix contained demo address", ip: "127.0.0.2"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fr := runnertest.New(t)
			scriptIPAddr(fr, "addr_multi.json")

			got, err := Discover(t.Context(), fr, netip.MustParseAddr(tc.ip),
				DiscoverOptions{SysfsRoot: newSysfs(t), AllowVirtual: true})
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			if got.Name != "lo" {
				t.Errorf("Name = %q, want lo", got.Name)
			}
			if !got.Class.Loopback {
				t.Errorf("Class.Loopback = false, want true (caller warns on it): %+v", got.Class)
			}
			if got.PrefixLen != 8 {
				t.Errorf("PrefixLen = %d, want 8", got.PrefixLen)
			}
		})
	}
}

func TestVirtualPatternsRejected(t *testing.T) {
	t.Run("docker0 bridge rejected without flag", func(t *testing.T) {
		fr := runnertest.New(t)
		scriptIPAddr(fr, "addr_multi.json")
		root := newSysfs(t)
		// Real bridges have a uevent DEVTYPE but no device symlink; the
		// missing-device rule fires first.
		setUevent(t, root, "docker0", "DEVTYPE=bridge\nINTERFACE=docker0\nIFINDEX=4\n")

		_, err := Discover(t.Context(), fr, netip.MustParseAddr("172.17.0.1"), DiscoverOptions{SysfsRoot: root})
		if !errors.Is(err, ErrNotPhysical) {
			t.Fatalf("err = %v, want ErrNotPhysical", err)
		}
		if !strings.Contains(err.Error(), "docker0") {
			t.Errorf("error %q does not name docker0", err)
		}
	})

	t.Run("wg0 link_type none rejected", func(t *testing.T) {
		fr := runnertest.New(t)
		scriptIPAddr(fr, "addr_multi.json")

		_, err := Discover(t.Context(), fr, netip.MustParseAddr("10.100.0.2"), DiscoverOptions{SysfsRoot: newSysfs(t)})
		if !errors.Is(err, ErrNotPhysical) {
			t.Fatalf("err = %v, want ErrNotPhysical", err)
		}
		for _, want := range []string{"wg0", "none"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q missing %q", err, want)
			}
		}
	})

	t.Run("wg0 allowed with flag so caller can warn", func(t *testing.T) {
		fr := runnertest.New(t)
		scriptIPAddr(fr, "addr_multi.json")

		got, err := Discover(t.Context(), fr, netip.MustParseAddr("10.100.0.2"),
			DiscoverOptions{SysfsRoot: newSysfs(t), AllowVirtual: true})
		if err != nil {
			t.Fatalf("Discover: %v", err)
		}
		if got.Name != "wg0" || !got.Class.Virtual {
			t.Errorf("got %+v, want wg0 with Class.Virtual=true", got)
		}
	})

	t.Run("wireless rejected without flag", func(t *testing.T) {
		fr := runnertest.New(t)
		scriptIPAddr(fr, "addr_multi.json")
		root := newSysfs(t)
		// A wifi NIC has a real device symlink; the wireless dir must win.
		setDevice(t, root, "wlan0", "pci0000:00/0000:00:14.3", "iwlwifi")
		setWireless(t, root, "wlan0")

		_, err := Discover(t.Context(), fr, netip.MustParseAddr("192.168.1.23"), DiscoverOptions{SysfsRoot: root})
		if !errors.Is(err, ErrNotPhysical) {
			t.Fatalf("err = %v, want ErrNotPhysical", err)
		}
		if !strings.Contains(err.Error(), "wireless") {
			t.Errorf("error %q does not mention wireless", err)
		}
	})

	t.Run("classify veth missing device symlink", func(t *testing.T) {
		root := newSysfs(t)
		ifaceDir(t, root, "veth1a2b")

		cls := Classify(root, "veth1a2b")
		if !cls.Virtual {
			t.Fatalf("Classify = %+v, want Virtual", cls)
		}
		if !strings.Contains(cls.Reason, "device") {
			t.Errorf("Reason %q should cite the missing device entry", cls.Reason)
		}
	})

	t.Run("classify uevent DEVTYPE with device present", func(t *testing.T) {
		root := newSysfs(t)
		setDevice(t, root, "eno1.100", "pci0000:00/0000:00:1f.6", "e1000e")
		setUevent(t, root, "eno1.100", "DEVTYPE=vlan\nINTERFACE=eno1.100\n")

		cls := Classify(root, "eno1.100")
		if !cls.Virtual {
			t.Fatalf("Classify = %+v, want Virtual", cls)
		}
		if !strings.Contains(cls.Reason, "vlan") {
			t.Errorf("Reason %q should cite DEVTYPE=vlan", cls.Reason)
		}
	})

	t.Run("classify wireless via DEVTYPE=wlan", func(t *testing.T) {
		root := newSysfs(t)
		setDevice(t, root, "wlp2s0", "pci0000:00/0000:02:00.0", "ath11k_pci")
		setUevent(t, root, "wlp2s0", "DEVTYPE=wlan\nINTERFACE=wlp2s0\n")

		cls := Classify(root, "wlp2s0")
		if !cls.Wireless || cls.Virtual {
			t.Errorf("Classify = %+v, want Wireless only", cls)
		}
	})

	t.Run("classify name prefix heuristics", func(t *testing.T) {
		// Device symlinks present so every earlier rule passes and only the
		// name-prefix belt-and-braces rule can fire.
		for _, name := range []string{"tailscale0", "br-lan", "virbr0", "tap3", "vnet7", "zt0"} {
			root := newSysfs(t)
			setDevice(t, root, name, "virtual/pseudo", "")
			setUevent(t, root, name, "INTERFACE="+name+"\n")

			cls := Classify(root, name)
			if !cls.Virtual {
				t.Errorf("Classify(%q) = %+v, want Virtual via name prefix", name, cls)
			}
		}
	})

	t.Run("classify loopback via sysfs type", func(t *testing.T) {
		root := newSysfs(t)
		setType(t, root, "lo", 772)

		cls := Classify(root, "lo")
		if !cls.Loopback {
			t.Errorf("Classify = %+v, want Loopback", cls)
		}
	})

	t.Run("classify non-ether sysfs type", func(t *testing.T) {
		root := newSysfs(t)
		setType(t, root, "wg0", 65534)

		cls := Classify(root, "wg0")
		if !cls.Virtual {
			t.Fatalf("Classify = %+v, want Virtual", cls)
		}
		if !strings.Contains(cls.Reason, "ethernet") {
			t.Errorf("Reason %q should say the link type is not ethernet", cls.Reason)
		}
	})

	t.Run("classify USB NIC stays physical with USB flag", func(t *testing.T) {
		root := newSysfs(t)
		setType(t, root, "enp0s20u1", 1)
		setDevice(t, root, "enp0s20u1", "pci0000:00/0000:00:14.0/usb1/1-2/1-2:1.0", "r8152")

		cls := Classify(root, "enp0s20u1")
		want := Class{USB: true, Driver: "r8152"}
		if cls != want {
			t.Errorf("Classify = %+v, want %+v", cls, want)
		}
	})
}

func TestExplicitInterfaceOverride(t *testing.T) {
	t.Run("interface owning the IP is accepted", func(t *testing.T) {
		fr := runnertest.New(t)
		scriptIPAddr(fr, "addr_multi.json")
		root := newSysfs(t)
		setDevice(t, root, "enp3s0", "pci0000:00/0000:00:1f.6", "e1000e")

		got, err := Discover(t.Context(), fr, netip.MustParseAddr("10.0.0.1"),
			DiscoverOptions{Interface: "enp3s0", SysfsRoot: root})
		if err != nil {
			t.Fatalf("Discover: %v", err)
		}
		if got.Name != "enp3s0" || got.PrefixLen != 24 {
			t.Errorf("got %+v, want enp3s0/24", got)
		}
	})

	t.Run("altname resolves to the canonical interface", func(t *testing.T) {
		fr := runnertest.New(t)
		scriptIPAddr(fr, "addr_single_25g.json")
		root := newSysfs(t)
		setDevice(t, root, "enp5s0", "pci0000:00/0000:05:00.0", "igc")

		got, err := Discover(t.Context(), fr, netip.MustParseAddr("192.168.50.2"),
			DiscoverOptions{Interface: "eth-lab-25g", SysfsRoot: root})
		if err != nil {
			t.Fatalf("Discover: %v", err)
		}
		if got.Name != "enp5s0" {
			t.Errorf("Name = %q, want enp5s0", got.Name)
		}
	})

	t.Run("interface not owning the IP is rejected", func(t *testing.T) {
		fr := runnertest.New(t)
		scriptIPAddr(fr, "addr_multi.json")

		_, err := Discover(t.Context(), fr, netip.MustParseAddr("10.0.0.1"),
			DiscoverOptions{Interface: "wlan0", SysfsRoot: newSysfs(t)})
		if !errors.Is(err, ErrIPNotAssigned) {
			t.Fatalf("err = %v, want ErrIPNotAssigned", err)
		}
		for _, want := range []string{"wlan0", "192.168.1.23"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q missing %q", err, want)
			}
		}
	})

	t.Run("unknown interface", func(t *testing.T) {
		fr := runnertest.New(t)
		scriptIPAddr(fr, "addr_multi.json")

		_, err := Discover(t.Context(), fr, netip.MustParseAddr("10.0.0.1"),
			DiscoverOptions{Interface: "enp99s0", SysfsRoot: newSysfs(t)})
		if !errors.Is(err, ErrInterfaceNotFound) {
			t.Fatalf("err = %v, want ErrInterfaceNotFound", err)
		}
	})

	t.Run("classification still validated", func(t *testing.T) {
		fr := runnertest.New(t)
		scriptIPAddr(fr, "addr_multi.json")
		root := newSysfs(t)
		setDevice(t, root, "wlan0", "pci0000:00/0000:00:14.3", "iwlwifi")
		setWireless(t, root, "wlan0")

		_, err := Discover(t.Context(), fr, netip.MustParseAddr("192.168.1.23"),
			DiscoverOptions{Interface: "wlan0", SysfsRoot: root})
		if !errors.Is(err, ErrNotPhysical) {
			t.Fatalf("err = %v, want ErrNotPhysical", err)
		}
	})
}

func TestOperstateDown(t *testing.T) {
	fr := runnertest.New(t)
	scriptIPAddr(fr, "addr_multi.json")
	root := newSysfs(t)
	setUevent(t, root, "docker0", "DEVTYPE=bridge\nINTERFACE=docker0\n")

	// docker0 is operstate DOWN in the fixture; AllowVirtual gets it past
	// classification so the down state itself is the surfaced failure.
	_, err := Discover(t.Context(), fr, netip.MustParseAddr("172.17.0.1"),
		DiscoverOptions{SysfsRoot: root, AllowVirtual: true})
	if !errors.Is(err, ErrInterfaceDown) {
		t.Fatalf("err = %v, want ErrInterfaceDown", err)
	}
	if !strings.Contains(err.Error(), "down") {
		t.Errorf("error %q does not mention the down operstate", err)
	}
}

func TestMalformedIPJSON(t *testing.T) {
	t.Run("garbage output", func(t *testing.T) {
		fr := runnertest.New(t)
		fr.Script(runnertest.Script{
			Name:   "ip",
			Match:  runnertest.ArgsExact("-j", "addr", "show"),
			Result: runner.CommandResult{Stdout: []byte("flim flam { not json")},
		})

		_, err := Discover(t.Context(), fr, netip.MustParseAddr("10.0.0.1"), DiscoverOptions{SysfsRoot: newSysfs(t)})
		if !errors.Is(err, ErrMalformedIPOutput) {
			t.Fatalf("err = %v, want ErrMalformedIPOutput", err)
		}
	})

	t.Run("ip exits non-zero", func(t *testing.T) {
		fr := runnertest.New(t)
		fr.Script(runnertest.Script{
			Name:  "ip",
			Match: runnertest.ArgsExact("-j", "addr", "show"),
			Result: runner.CommandResult{
				ExitCode: 1,
				Stderr:   []byte("Cannot open netlink socket: Permission denied"),
			},
		})

		_, err := Discover(t.Context(), fr, netip.MustParseAddr("10.0.0.1"), DiscoverOptions{SysfsRoot: newSysfs(t)})
		if err == nil {
			t.Fatal("Discover succeeded, want error for non-zero ip exit")
		}
		if !strings.Contains(err.Error(), "netlink") {
			t.Errorf("error %q does not carry the ip stderr", err)
		}
	})
}
