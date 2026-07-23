package reporting

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRegeneratePure pins the property that makes `cablecheck report` (offline
// regeneration) correct: rendering is a pure function of the Report, so two
// calls on the same value are byte-identical.
func TestRegeneratePure(t *testing.T) {
	r := goldenReport()

	html1 := RenderHTML(r)
	html2 := RenderHTML(r)
	if !bytes.Equal(html1, html2) {
		t.Errorf("RenderHTML is not pure: two calls on the same report differ")
	}

	md1 := RenderMarkdown(r)
	md2 := RenderMarkdown(r)
	if !bytes.Equal(md1, md2) {
		t.Errorf("RenderMarkdown is not pure: two calls on the same report differ")
	}

	sum1 := RenderSummary(r)
	sum2 := RenderSummary(r)
	if !bytes.Equal(sum1, sum2) {
		t.Errorf("RenderSummary is not pure: two calls on the same report differ")
	}

	js1, err := RenderJSON(r)
	if err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	js2, err := RenderJSON(r)
	if err != nil {
		t.Fatalf("RenderJSON (second call): %v", err)
	}
	if !bytes.Equal(js1, js2) {
		t.Errorf("RenderJSON is not pure: two calls on the same report differ")
	}
}

// TestReportingDependencyPurity enforces the package contract structurally:
// internal/reporting may depend on no internal package except model, so
// report regeneration can never grow hidden coupling.
func TestReportingDependencyPurity(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "./internal/reporting")
	cmd.Dir = filepath.Join("..", "..")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	allowed := map[string]bool{
		"cablecheck/internal/model":     true,
		"cablecheck/internal/reporting": true,
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		pkg := strings.TrimSpace(line)
		if strings.HasPrefix(pkg, "cablecheck/") && !allowed[pkg] {
			t.Errorf("internal/reporting depends on %s; only cablecheck/internal/model is allowed", pkg)
		}
	}
}
