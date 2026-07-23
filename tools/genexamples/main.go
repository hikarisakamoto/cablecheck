// Command genexamples writes the committed examples/ reports for CableCheck's
// five canonical scenarios (healthy, reduced-speed, crc-errors, host-limited,
// failed).
//
// Each scenario is a pre-evaluation model.Report seed built from values
// traceable to the testdata fixtures. The generator runs the real pipeline —
// evaluate.FactsFromReport -> evaluate.Evaluate -> reporting.Render* — so the
// artifacts can never diverge from the live classification and rendering code.
// A drift test (TestExamplesUpToDate) regenerates into a temp directory and
// byte-compares, so `make examples` must be rerun after any evaluator or
// reporting change.
//
// Usage:
//
//	go run ./tools/genexamples -out examples
//
// It is deterministic by construction: fixed timestamps (2026-01-02T15:04:05Z),
// fixed testIds (example-<name>), no clock, no randomness, no token anywhere.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"cablecheck/internal/evaluate"
	"cablecheck/internal/model"
	"cablecheck/internal/reporting"
)

// exampleFiles are the artifacts written per scenario, in a fixed order.
var exampleFiles = []string{"report.json", "report.md", "summary.txt", "report.html"}

func main() {
	out := flag.String("out", "examples", "output directory for the generated example scenarios")
	flag.Parse()
	if err := generateInto(*out); err != nil {
		fmt.Fprintf(os.Stderr, "genexamples: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %d example scenarios to %s\n", len(seeds()), *out)
}

// evaluated runs the real evaluation pipeline over a seed report and folds the
// classification, score, findings, recommendations and reasons back into a copy
// of the report exactly the way app.finish does for a live run. It never
// mutates the seed.
func evaluated(seed *model.Report) *model.Report {
	rep := *seed // shallow copy: the pipeline only sets scalar/slice result fields
	res := evaluate.Evaluate(evaluate.FactsFromReport(&rep))
	rep.Classification = res.Class
	rep.Score = res.Score
	rep.Findings = res.Findings
	rep.Recommendations = res.Recommendations
	rep.ClassificationReasons = nil
	for _, f := range res.Findings {
		rep.ClassificationReasons = append(rep.ClassificationReasons, f.Text)
	}
	return &rep
}

// generateInto evaluates and renders every scenario into base/<name>/, creating
// the directories with 0700 permissions. Each scenario yields report.json,
// report.md, summary.txt and report.html.
func generateInto(base string) error {
	for _, sc := range seeds() {
		dir := filepath.Join(base, sc.Name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
		rep := evaluated(sc.Report)
		jsonBytes, err := reporting.RenderJSON(rep)
		if err != nil {
			return fmt.Errorf("render json for %s: %w", sc.Name, err)
		}
		files := map[string][]byte{
			"report.json": jsonBytes,
			"report.md":   reporting.RenderMarkdown(rep),
			"summary.txt": reporting.RenderSummary(rep),
			"report.html": reporting.RenderHTML(rep),
		}
		for _, name := range exampleFiles {
			dst := filepath.Join(dir, name)
			if err := os.WriteFile(dst, files[name], 0o600); err != nil {
				return fmt.Errorf("write %s: %w", dst, err)
			}
		}
	}
	return nil
}
