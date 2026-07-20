package network

import (
	"errors"
	"net/netip"
	"strings"
	"testing"

	"cablecheck/internal/runner"
	"cablecheck/internal/runner/runnertest"
)

func TestInferInterfaceIPv4(t *testing.T) {
	t.Run("single ipv4 is inferred", func(t *testing.T) {
		fr := runnertest.New(t)
		scriptIPAddr(fr, "addr_multi.json")
		got, err := InferInterfaceIPv4(t.Context(), fr, "enp3s0")
		if err != nil {
			t.Fatalf("InferInterfaceIPv4: %v", err)
		}
		if got != netip.MustParseAddr("10.0.0.1") {
			t.Errorf("got %v, want 10.0.0.1", got)
		}
	})

	t.Run("no ipv4 is an error", func(t *testing.T) {
		fr := runnertest.New(t)
		scriptIPAddr(fr, "addr_multi.json")
		_, err := InferInterfaceIPv4(t.Context(), fr, "veth1a2b")
		if err == nil || !strings.Contains(err.Error(), "no IPv4") {
			t.Errorf("err = %v, want a 'no IPv4 address' error", err)
		}
	})

	t.Run("unknown interface", func(t *testing.T) {
		fr := runnertest.New(t)
		scriptIPAddr(fr, "addr_multi.json")
		_, err := InferInterfaceIPv4(t.Context(), fr, "eth9")
		if !errors.Is(err, ErrInterfaceNotFound) {
			t.Errorf("err = %v, want ErrInterfaceNotFound", err)
		}
	})

	t.Run("multiple ipv4 is ambiguous", func(t *testing.T) {
		fr := runnertest.New(t)
		const dual = `[{"ifname":"eth0","link_type":"ether","addr_info":[` +
			`{"family":"inet","local":"10.0.0.1","prefixlen":24},` +
			`{"family":"inet","local":"10.0.0.2","prefixlen":24}]}]`
		fr.Script(runnertest.Script{
			Name:   "ip",
			Match:  runnertest.ArgsExact("-j", "addr", "show"),
			Result: runner.CommandResult{Stdout: []byte(dual)},
		})
		_, err := InferInterfaceIPv4(t.Context(), fr, "eth0")
		if err == nil || !strings.Contains(err.Error(), "pass --local-ip to choose") {
			t.Errorf("err = %v, want an ambiguity error", err)
		}
	})
}
