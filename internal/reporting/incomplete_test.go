package reporting

import (
	"strings"
	"testing"
	"time"

	"cablecheck/internal/model"
)

// TestIncompleteTCPRendersNotMeasured pins that a TCP run marked Incomplete is
// labeled and blanked to n/a across every surface, never surfacing its
// untrusted leftover throughput or CPU as if it were a measurement. This is the
// rendering counterpart to the evaluator's incomplete-TCP handling: the
// evaluator already refuses to score such a run, and the report must not
// present it as a real 0.
func TestIncompleteTCPRendersNotMeasured(t *testing.T) {
	r := &model.Report{
		Tests: model.TestsSection{
			TCP: []model.TCPResult{{
				Direction:             model.DirectionPC1ToPC2,
				Incomplete:            true,
				ParallelStreams:       1,
				Duration:              model.Duration(10 * time.Second),
				SenderBitsPerSecond:   111_222_333, // untrusted leftover — must never surface
				ReceiverBitsPerSecond: 111_222_333,
				CPUUtilization:        model.CPUUsage{HostTotal: 77.7, RemoteTotal: 66.6},
			}},
		},
	}
	badBps := fmtBps(111_222_333)
	surfaces := map[string]string{
		"markdown": string(RenderMarkdown(r)),
		"html":     string(RenderHTML(r)),
		"summary":  string(RenderSummary(r)),
	}
	for name, out := range surfaces {
		if strings.Contains(out, badBps) || strings.Contains(out, "77.7") || strings.Contains(out, "66.6") {
			t.Errorf("%s surfaced an incomplete run's untrusted metrics (%q / 77.7 / 66.6):\n%s", name, badBps, out)
		}
	}
	if md := surfaces["markdown"]; !strings.Contains(md, "1 (incomplete)") ||
		!strings.Contains(md, "TCP "+dirLabel(model.DirectionPC1ToPC2)+" (incomplete)") {
		t.Errorf("markdown did not label the incomplete TCP throughput and CPU rows:\n%s", md)
	}
	if html := surfaces["html"]; !strings.Contains(html, "(incomplete)") {
		t.Errorf("html did not label the incomplete TCP run:\n%s", html)
	}
	if s := surfaces["summary"]; !strings.Contains(s, "n/a (incomplete)") {
		t.Errorf("summary did not mark TCP throughput incomplete:\n%s", s)
	}
}
