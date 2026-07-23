package ui

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"

	"cablecheck/internal/model"
)

func TestRendererSummaryPlainOutput(t *testing.T) {
	rep := presentationReport()
	var out bytes.Buffer
	r := newRenderer(&out, Options{Color: ColorNever, Width: 120}, false)
	r.Summary(rep, "/tmp/cablecheck-report")

	got := out.String()
	for _, want := range []string{
		"CableCheck result",
		"cable health: GOOD  Score: 88/100",
		"The cable is healthy; only minor deviations were observed.",
		"Link: 1000 Mb/s full duplex  (pc1 eth0, pc2 eno1)",
		"Ping loss:       pc1->pc2 0.25%  |  pc2->pc1 0.00%",
		"TCP throughput:  pc1->pc2 941.0 Mbit/s  |  pc2->pc1 932.0 Mbit/s",
		"TCP retransmits: pc1->pc2 2  |  pc2->pc1 n/a",
		"UDP loss:        pc1->pc2 0.10%  |  pc2->pc1 n/a",
		"Findings (top 3 of 4):",
		"[Warning] PHY-01: first finding",
		"Recommendations (top 3 of 4):",
		"Report: /tmp/cablecheck-report",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q:\n%s", want, got)
		}
	}
	for _, excluded := range []string{"fourth finding", "fourth recommendation", "\x1b["} {
		if strings.Contains(got, excluded) {
			t.Errorf("summary unexpectedly contains %q:\n%s", excluded, got)
		}
	}
}

func TestRendererSummaryPartialAndColor(t *testing.T) {
	rep := presentationReport()
	rep.Partial = true
	var out bytes.Buffer
	r := newRenderer(&out, Options{Color: ColorAlways, Width: 120}, false)
	r.Summary(rep, "/reports/run")

	got := out.String()
	if !strings.Contains(got, "cable health:") || !strings.Contains(got, "(PARTIAL RUN)") {
		t.Fatalf("colored partial summary lost required text: %q", got)
	}
	if !strings.Contains(got, "\x1b[32mcable health: GOOD") {
		t.Errorf("health line was not colored green: %q", got)
	}
	if !strings.Contains(got, "\x1b[33m[Warning] PHY-01") {
		t.Errorf("warning finding was not colored yellow: %q", got)
	}
}

func TestRendererSummaryRespectsConfiguredWidth(t *testing.T) {
	rep := presentationReport()
	rep.Recommendations = []string{"a deliberately long recommendation that must wrap without being discarded"}
	var out bytes.Buffer
	r := newRenderer(&out, Options{Color: ColorNever, Width: 40}, false)
	r.Summary(rep, "/reports/a/deliberately/long/path/that/must/wrap")

	for _, line := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
		if width := utf8.RuneCountInString(line); width > 40 {
			t.Errorf("line width = %d, want <= 40: %q", width, line)
		}
	}
	var compact strings.Builder
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "| ") && strings.HasSuffix(line, " |") {
			content := strings.TrimSuffix(strings.TrimPrefix(line, "| "), " |")
			compact.WriteString(strings.ReplaceAll(strings.TrimRight(content, " "), " ", ""))
		}
	}
	for _, want := range []string{
		"-adeliberatelylongrecommendationthatmustwrapwithoutbeingdiscarded",
		"Report:/reports/a/deliberately/long/path/that/must/wrap",
	} {
		if !strings.Contains(compact.String(), want) {
			t.Errorf("wrapped summary lost %q:\n%s", want, out.String())
		}
	}
}

func TestRendererTokenBannerPreservesCommand(t *testing.T) {
	command := "cablecheck run --role pc2 --local-ip 192.168.50.2 --peer-ip 192.168.50.1 --token 123456"
	var out bytes.Buffer
	r := newRenderer(&out, Options{Color: ColorNever, Width: 40}, false)
	r.TokenBanner("123456", command, true)

	got := out.String()
	for _, want := range []string{"Session token: 123456  (auto-generated)", "On PC2 run:", command} {
		if !strings.Contains(got, want) {
			t.Errorf("token banner missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\x1b[") {
		t.Errorf("plain token banner emitted ANSI: %q", got)
	}
}

func TestRendererWorkerSummary(t *testing.T) {
	for _, tc := range []struct {
		name        string
		transferred bool
		path        string
		wantLabel   string
	}{
		{name: "transferred", transferred: true, path: "/reports/run", wantLabel: "Report received from PC1:"},
		{name: "fallback", path: "/reports/run/summary.txt", wantLabel: "Summary:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			r := newRenderer(&out, Options{Color: ColorNever}, false)
			r.WorkerSummary(model.HealthExcellent, "verdict from PC1: cable health: EXCELLENT", tc.path, tc.transferred)
			got := out.String()
			if !strings.Contains(got, "verdict from PC1:") || !strings.Contains(got, tc.wantLabel+" "+tc.path) {
				t.Fatalf("worker summary = %q", got)
			}
		})
	}
}

func presentationReport() *model.Report {
	score := 88
	retransmits := uint64(2)
	return &model.Report{
		Classification: model.HealthGood,
		Score:          &score,
		PC1:            model.PeerReport{NIC: model.NICReport{Name: "eth0"}},
		PC2:            model.PeerReport{NIC: model.NICReport{Name: "eno1"}},
		Link: &model.LinkSection{PC1: model.LinkEndpoint{Before: &model.LinkSettings{
			SpeedMbps: 1000, Duplex: "full", LinkDetected: true,
		}}},
		Tests: model.TestsSection{
			Ping: []model.PingResult{
				{Direction: model.DirectionPC1ToPC2, LossPercent: 0.25},
				{Direction: model.DirectionPC2ToPC1, LossPercent: 0},
			},
			TCP: []model.TCPResult{
				{Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 941_000_000, Retransmissions: &retransmits},
				{Direction: model.DirectionPC2ToPC1, ReceiverBitsPerSecond: 932_000_000},
			},
			UDP: []model.UDPResult{{Direction: model.DirectionPC1ToPC2, LossPercent: 0.1}},
		},
		Findings: []model.Finding{
			{Severity: model.SevWarning, RuleID: "PHY-01", Text: "first finding"},
			{Severity: model.SevInfo, RuleID: "PERF-01", Text: "second finding"},
			{Severity: model.SevMarker, RuleID: "LIM-01", Text: "third finding"},
			{Severity: model.SevPoor, RuleID: "PHY-04", Text: "fourth finding"},
		},
		Recommendations: []string{
			"first recommendation", "second recommendation", "third recommendation", "fourth recommendation",
		},
	}
}
