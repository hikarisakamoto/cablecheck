package parser

import (
	"errors"
	"slices"
	"testing"
)

// inet returns the first IPv4 addr_info entry of a link, or nil.
func inet(l IPLink) *IPAddrInfo {
	for i := range l.AddrInfo {
		if l.AddrInfo[i].Family == "inet" {
			return &l.AddrInfo[i]
		}
	}
	return nil
}

func TestIPAddrParse(t *testing.T) {
	t.Run("multi", func(t *testing.T) {
		links, err := ParseIPAddr(fixture(t, "ip", "addr_multi.json"))
		if err != nil {
			t.Fatalf("ParseIPAddr: %v", err)
		}
		var names []string
		byName := map[string]IPLink{}
		for _, l := range links {
			names = append(names, l.Ifname)
			byName[l.Ifname] = l
		}
		wantNames := []string{"lo", "enp3s0", "wlan0", "docker0", "veth1a2b", "wg0"}
		if !slices.Equal(names, wantNames) {
			t.Fatalf("interface names = %v, want %v", names, wantNames)
		}

		lo := byName["lo"]
		if lo.LinkType != "loopback" {
			t.Errorf("lo.LinkType = %q, want loopback", lo.LinkType)
		}
		if !slices.Contains(lo.Flags, "LOOPBACK") {
			t.Errorf("lo.Flags = %v, want to contain LOOPBACK", lo.Flags)
		}

		e := byName["enp3s0"]
		if e.Ifindex != 2 {
			t.Errorf("enp3s0.Ifindex = %d, want 2", e.Ifindex)
		}
		if e.MTU != 1500 {
			t.Errorf("enp3s0.MTU = %d, want 1500", e.MTU)
		}
		if e.LinkType != "ether" {
			t.Errorf("enp3s0.LinkType = %q, want ether", e.LinkType)
		}
		if e.Operstate != "up" {
			t.Errorf("enp3s0.Operstate = %q, want %q (normalized lowercase)", e.Operstate, "up")
		}
		for _, f := range []string{"BROADCAST", "UP", "LOWER_UP"} {
			if !slices.Contains(e.Flags, f) {
				t.Errorf("enp3s0.Flags = %v, want to contain %s", e.Flags, f)
			}
		}
		a := inet(e)
		if a == nil {
			t.Fatal("enp3s0 has no inet addr_info")
		}
		if a.Local != "10.0.0.1" || a.Prefixlen != 24 || a.Scope != "global" || a.Label != "enp3s0" {
			t.Errorf("enp3s0 inet = %+v, want 10.0.0.1/24 global label enp3s0", *a)
		}

		w := byName["wlan0"]
		if a := inet(w); a == nil || !a.Dynamic {
			t.Errorf("wlan0 inet = %+v, want Dynamic=true", a)
		}

		d := byName["docker0"]
		if d.Operstate != "down" {
			t.Errorf("docker0.Operstate = %q, want down", d.Operstate)
		}
		if !slices.Contains(d.Flags, "NO-CARRIER") {
			t.Errorf("docker0.Flags = %v, want to contain NO-CARRIER", d.Flags)
		}

		v := byName["veth1a2b"]
		if a := inet(v); a != nil {
			t.Errorf("veth1a2b inet = %+v, want no IPv4 address", *a)
		}

		g := byName["wg0"]
		if g.LinkType != "none" {
			t.Errorf(`wg0.LinkType = %q, want "none" (WireGuard)`, g.LinkType)
		}
		if a := inet(g); a == nil || a.Local != "10.100.0.2" || a.Prefixlen != 32 {
			t.Errorf("wg0 inet = %+v, want 10.100.0.2/32", a)
		}
	})

	t.Run("single_25g_altnames", func(t *testing.T) {
		links, err := ParseIPAddr(fixture(t, "ip", "addr_single_25g.json"))
		if err != nil {
			t.Fatalf("ParseIPAddr: %v", err)
		}
		if len(links) != 1 {
			t.Fatalf("got %d links, want 1", len(links))
		}
		l := links[0]
		if l.Ifname != "enp5s0" {
			t.Errorf("Ifname = %q, want enp5s0", l.Ifname)
		}
		if !slices.Contains(l.Altnames, "enx24418c9f0a11") {
			t.Errorf("Altnames = %v, want to contain enx24418c9f0a11", l.Altnames)
		}
		if a := inet(l); a == nil || a.Local != "192.168.50.2" || a.Prefixlen != 24 {
			t.Errorf("inet = %+v, want 192.168.50.2/24", a)
		}
	})

	t.Run("ip_not_found_shape", func(t *testing.T) {
		links, err := ParseIPAddr(fixture(t, "ip", "addr_ip_not_found.json"))
		if err != nil {
			t.Fatalf("ParseIPAddr: %v", err)
		}
		if len(links) != 2 {
			t.Fatalf("got %d links, want 2", len(links))
		}
		if a := inet(links[1]); a == nil || a.Local != "10.0.0.5" || a.Prefixlen != 24 {
			t.Errorf("enp9s0 inet = %+v, want 10.0.0.5/24 (owns the subnet, not the target)", a)
		}
	})

	t.Run("malformed", func(t *testing.T) {
		if _, err := ParseIPAddr([]byte("not json")); err == nil {
			t.Error("ParseIPAddr(garbage) = nil error, want error")
		}
	})
}

func TestIPLinkStats(t *testing.T) {
	t.Run("clean", func(t *testing.T) {
		l, err := ParseIPLinkStats(fixture(t, "ip", "linkstats_clean.json"))
		if err != nil {
			t.Fatalf("ParseIPLinkStats: %v", err)
		}
		s := l.Stats64
		if s == nil {
			t.Fatal("Stats64 = nil, want populated")
		}
		if s.RX.Bytes != 61250873344 || s.RX.Packets != 48291733 {
			t.Errorf("rx bytes/packets = %d/%d, want 61250873344/48291733", s.RX.Bytes, s.RX.Packets)
		}
		if s.RX.CRCErrors != 0 || s.RX.FrameErrors != 0 || s.TX.CarrierErrors != 0 {
			t.Errorf("clean fixture has nonzero error counters: %+v %+v", s.RX, s.TX)
		}
		if s.TX.CarrierChanges != 2 {
			t.Errorf("tx.carrier_changes = %d, want 2 (flat under tx)", s.TX.CarrierChanges)
		}
	})

	t.Run("errors_flat_fields", func(t *testing.T) {
		l, err := ParseIPLinkStats(fixture(t, "ip", "linkstats_errors.json"))
		if err != nil {
			t.Fatalf("ParseIPLinkStats: %v", err)
		}
		if l.Operstate != "up" {
			t.Errorf("Operstate = %q, want %q (normalized lowercase)", l.Operstate, "up")
		}
		s := l.Stats64
		if s == nil {
			t.Fatal("Stats64 = nil, want populated")
		}
		if s.RX.CRCErrors != 1543 {
			t.Errorf("rx.crc_errors = %d, want 1543", s.RX.CRCErrors)
		}
		if s.RX.FrameErrors != 12 {
			t.Errorf("rx.frame_errors = %d, want 12", s.RX.FrameErrors)
		}
		if s.RX.MissedErrors != 9 {
			t.Errorf("rx.missed_errors = %d, want 9", s.RX.MissedErrors)
		}
		if s.RX.FifoErrors != 3 {
			t.Errorf("rx.fifo_errors = %d, want 3", s.RX.FifoErrors)
		}
		if s.TX.CarrierErrors != 2 {
			t.Errorf("tx.carrier_errors = %d, want 2", s.TX.CarrierErrors)
		}
		if s.TX.Collisions != 7 {
			t.Errorf("tx.collisions = %d, want 7", s.TX.Collisions)
		}
		if s.TX.CarrierChanges != 5 {
			t.Errorf("tx.carrier_changes = %d, want 5", s.TX.CarrierChanges)
		}
	})

	t.Run("exactly_one_element", func(t *testing.T) {
		if _, err := ParseIPLinkStats([]byte(`[]`)); err == nil {
			t.Error("empty array accepted, want error")
		}
		two := `[{"ifindex":1,"ifname":"a","stats64":{"rx":{},"tx":{}}},` +
			`{"ifindex":2,"ifname":"b","stats64":{"rx":{},"tx":{}}}]`
		if _, err := ParseIPLinkStats([]byte(two)); err == nil {
			t.Error("two-element array accepted, want error")
		}
	})

	t.Run("nil_stats64_typed_error", func(t *testing.T) {
		_, err := ParseIPLinkStats([]byte(`[{"ifindex":2,"ifname":"enp3s0","operstate":"UP"}]`))
		if !errors.Is(err, ErrNoStats64) {
			t.Errorf("err = %v, want errors.Is(err, ErrNoStats64)", err)
		}
	})

	t.Run("malformed", func(t *testing.T) {
		if _, err := ParseIPLinkStats([]byte("{")); err == nil {
			t.Error("ParseIPLinkStats(garbage) = nil error, want error")
		}
	})
}
