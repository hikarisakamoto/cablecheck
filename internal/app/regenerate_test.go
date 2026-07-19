package app

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cablecheck/internal/model"
	"cablecheck/internal/reporting"
)

// regenReport builds a small but complete report for the regeneration tests:
// the reporting layer renders every section, so a minimal record suffices to
// prove byte-identical re-rendering.
func regenReport() *model.Report {
	started := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)
	finished := started.Add(45 * time.Second)
	score := 97
	return &model.Report{
		SchemaVersion:   model.SchemaVersion,
		ToolVersion:     "1.0.0",
		ProtocolVersion: "1",
		TestID:          "example-regen",
		StartedAt:       started,
		FinishedAt:      finished,
		Duration:        model.Duration(45 * time.Second),
		Configuration: model.ConfigEcho{
			Role:    "pc1",
			LocalIP: "192.168.100.1",
			PeerIP:  "192.168.100.2",
			Mode:    "quick",
		},
		PC1: model.PeerReport{Hostname: "alpha", NIC: model.NICReport{Name: "enp3s0", Driver: "e1000e", SpeedMbps: 1000, Duplex: "full"}},
		PC2: model.PeerReport{Hostname: "bravo", NIC: model.NICReport{Name: "enp4s0", Driver: "e1000e", SpeedMbps: 1000, Duplex: "full"}},
		Tests: model.TestsSection{
			TCP: []model.TCPResult{
				{Direction: model.DirectionPC1ToPC2, SenderBitsPerSecond: 941e6, ReceiverBitsPerSecond: 940e6, Duration: model.Duration(30 * time.Second)},
				{Direction: model.DirectionPC2ToPC1, SenderBitsPerSecond: 939e6, ReceiverBitsPerSecond: 938e6, Duration: model.Duration(30 * time.Second)},
			},
		},
		Classification:        model.HealthExcellent,
		Score:                 &score,
		ClassificationReasons: []string{"all measured tests passed"},
		Recommendations:       nil,
	}
}

// writeReportJSON marshals rep to <dir>/report.json and returns the path.
func writeReportJSON(t *testing.T, dir string, rep *model.Report) string {
	t.Helper()
	data, err := reporting.RenderJSON(rep)
	if err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	path := filepath.Join(dir, "report.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write report.json: %v", err)
	}
	return path
}

func TestRegenerateFromJSON(t *testing.T) {
	t.Run("ByteIdenticalMarkdownAndSummary", func(t *testing.T) {
		rep := regenReport()
		dir := t.TempDir()
		path := writeReportJSON(t, dir, rep)

		var stdout bytes.Buffer
		if err := Regenerate(path, dir, &stdout); err != nil {
			t.Fatalf("Regenerate: %v", err)
		}

		// The re-rendered files must equal a direct render of the same report:
		// regeneration re-parses the JSON and renders through the same pure
		// functions, so any divergence is a bug.
		gotMD, err := os.ReadFile(filepath.Join(dir, "report.md"))
		if err != nil {
			t.Fatalf("read regenerated report.md: %v", err)
		}
		gotSum, err := os.ReadFile(filepath.Join(dir, "summary.txt"))
		if err != nil {
			t.Fatalf("read regenerated summary.txt: %v", err)
		}
		if wantMD := reporting.RenderMarkdown(rep); !bytes.Equal(gotMD, wantMD) {
			t.Errorf("regenerated report.md differs from a direct render")
		}
		if wantSum := reporting.RenderSummary(rep); !bytes.Equal(gotSum, wantSum) {
			t.Errorf("regenerated summary.txt differs from a direct render")
		}
	})

	t.Run("OutputDirDefaultsNextToJSON", func(t *testing.T) {
		rep := regenReport()
		dir := t.TempDir()
		path := writeReportJSON(t, dir, rep)
		var stdout bytes.Buffer
		// Empty outDir means "next to the JSON".
		if err := Regenerate(path, "", &stdout); err != nil {
			t.Fatalf("Regenerate: %v", err)
		}
		for _, name := range []string{"report.md", "summary.txt"} {
			if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
				t.Errorf("%s was not written next to the JSON: %v", name, err)
			}
		}
	})

	t.Run("SchemaMajorMismatchIsExitConfig", func(t *testing.T) {
		rep := regenReport()
		rep.SchemaVersion = "2.0.0"
		dir := t.TempDir()
		path := writeReportJSON(t, dir, rep)
		var stdout bytes.Buffer
		err := Regenerate(path, dir, &stdout)
		assertExitConfig(t, err)
		if !strings.Contains(err.Error(), "schema") {
			t.Errorf("error %q does not mention the schema version", err)
		}
		// No files written for an unsupported schema.
		if _, statErr := os.Stat(filepath.Join(dir, "report.md")); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("report.md was written despite the schema mismatch")
		}
	})

	t.Run("MinorSchemaBumpAccepted", func(t *testing.T) {
		// A same-major, higher-minor schema is forward-compatible.
		rep := regenReport()
		rep.SchemaVersion = "1.5.0"
		dir := t.TempDir()
		path := writeReportJSON(t, dir, rep)
		var stdout bytes.Buffer
		if err := Regenerate(path, dir, &stdout); err != nil {
			t.Errorf("Regenerate rejected a same-major schema bump: %v", err)
		}
	})

	t.Run("OversizeFileRejected", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "report.json")
		// A file just over the 64 MiB cap must be refused before parsing.
		big := make([]byte, regenSizeCap+1)
		big[0] = '['
		if err := os.WriteFile(path, big, 0o600); err != nil {
			t.Fatalf("write oversize report.json: %v", err)
		}
		var stdout bytes.Buffer
		err := Regenerate(path, dir, &stdout)
		assertExitConfig(t, err)
		if !strings.Contains(strings.ToLower(err.Error()), "large") &&
			!strings.Contains(strings.ToLower(err.Error()), "cap") &&
			!strings.Contains(strings.ToLower(err.Error()), "big") {
			t.Errorf("oversize error %q does not explain the size cap", err)
		}
	})

	t.Run("MissingFileIsExitConfig", func(t *testing.T) {
		var stdout bytes.Buffer
		err := Regenerate(filepath.Join(t.TempDir(), "nope.json"), "", &stdout)
		assertExitConfig(t, err)
	})

	t.Run("MalformedJSONIsExitConfig", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "report.json")
		if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
			t.Fatalf("write malformed json: %v", err)
		}
		var stdout bytes.Buffer
		assertExitConfig(t, Regenerate(path, dir, &stdout))
	})
}

func TestReadRegenerateDataOverCapIsExitConfig(t *testing.T) {
	_, err := readRegenerateData("report.json", strings.NewReader("123456789"), 8)
	assertExitConfig(t, err)
}

func TestRegenerateRejectsNonRegularInput(t *testing.T) {
	var stdout bytes.Buffer
	err := Regenerate(t.TempDir(), "", &stdout)
	assertExitConfig(t, err)
	if !strings.Contains(err.Error(), "regular file") {
		t.Errorf("non-regular input error = %q, want it to mention a regular file", err)
	}
}

func TestRegenerateOutputWriteFailureIsExitConfig(t *testing.T) {
	dir := t.TempDir()
	path := writeReportJSON(t, dir, regenReport())
	outDir := filepath.Join(dir, "does-not-exist")

	var stdout bytes.Buffer
	assertExitConfig(t, Regenerate(path, outDir, &stdout))
}
