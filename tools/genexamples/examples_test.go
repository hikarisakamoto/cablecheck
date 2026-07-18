package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"cablecheck/internal/app"
	"cablecheck/internal/model"
)

// examplesDir is the committed examples/ tree, relative to this package.
const examplesDir = "../../examples"

// TestExamplesUpToDate regenerates every scenario into a temp directory and
// byte-compares each produced file against the committed examples/ tree. It is
// the drift guard: any change to the evaluator or the reporting layer that is
// not accompanied by a fresh `make examples` fails here.
func TestExamplesUpToDate(t *testing.T) {
	tmp := t.TempDir()
	if err := generateInto(tmp); err != nil {
		t.Fatalf("generateInto(tmp): %v", err)
	}
	for _, sc := range seeds() {
		for _, file := range exampleFiles {
			rel := filepath.Join(sc.Name, file)
			got, err := os.ReadFile(filepath.Join(tmp, rel))
			if err != nil {
				t.Errorf("read generated %s: %v", rel, err)
				continue
			}
			want, err := os.ReadFile(filepath.Join(examplesDir, rel))
			if err != nil {
				t.Errorf("read committed %s: %v (run: make examples)", rel, err)
				continue
			}
			if !bytes.Equal(got, want) {
				t.Errorf("examples/%s is out of date — run: make examples", rel)
			}
		}
	}
}

// TestExampleClassifications pins each scenario's outcome so a seed edit cannot
// silently drift the classification the example is meant to demonstrate.
func TestExampleClassifications(t *testing.T) {
	cases := map[string]struct {
		wantClasses  []model.HealthClass // any of these is acceptable
		wantMinScore int                 // 0 = no lower bound
		wantNilScore bool
	}{
		"healthy":       {wantClasses: []model.HealthClass{model.HealthExcellent, model.HealthGood}, wantMinScore: 80},
		"reduced-speed": {wantClasses: []model.HealthClass{model.HealthWarning}},
		"crc-errors":    {wantClasses: []model.HealthClass{model.HealthPoor, model.HealthFailed}},
		"host-limited":  {wantClasses: []model.HealthClass{model.HealthInconclusive}, wantNilScore: true},
		"failed":        {wantClasses: []model.HealthClass{model.HealthFailed}},
	}
	byName := map[string]*model.Report{}
	for _, sc := range seeds() {
		rep := evaluated(sc.Report)
		byName[sc.Name] = rep
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			rep, ok := byName[name]
			if !ok {
				t.Fatalf("no seed named %q", name)
			}
			if !containsClass(want.wantClasses, rep.Classification) {
				t.Fatalf("class = %s, want one of %v", rep.Classification, want.wantClasses)
			}
			if want.wantNilScore {
				if rep.Score != nil {
					t.Errorf("score = %d, want nil for %s", *rep.Score, rep.Classification)
				}
			} else {
				if rep.Score == nil {
					t.Fatalf("score is nil, want a number for %s", rep.Classification)
				}
				if want.wantMinScore > 0 && *rep.Score < want.wantMinScore {
					t.Errorf("score = %d, want >= %d", *rep.Score, want.wantMinScore)
				}
			}
		})
	}
	// Every named scenario must have a pinned expectation and vice versa.
	if len(byName) != len(cases) {
		t.Errorf("seed count %d != pinned-case count %d", len(byName), len(cases))
	}
}

// TestExamplesRegenerate asserts every committed report.json survives an
// app.Regenerate round-trip byte-identically: the regenerated report.md and
// summary.txt must equal the committed copies, proving the example JSON is a
// faithful, self-describing record.
func TestExamplesRegenerate(t *testing.T) {
	for _, sc := range seeds() {
		t.Run(sc.Name, func(t *testing.T) {
			dir := filepath.Join(examplesDir, sc.Name)
			jsonPath := filepath.Join(dir, "report.json")

			wantMD, err := os.ReadFile(filepath.Join(dir, "report.md"))
			if err != nil {
				t.Fatalf("read committed report.md: %v", err)
			}
			wantSummary, err := os.ReadFile(filepath.Join(dir, "summary.txt"))
			if err != nil {
				t.Fatalf("read committed summary.txt: %v", err)
			}

			out := t.TempDir()
			var buf bytes.Buffer
			if err := app.Regenerate(jsonPath, out, &buf); err != nil {
				t.Fatalf("app.Regenerate: %v", err)
			}
			gotMD, err := os.ReadFile(filepath.Join(out, "report.md"))
			if err != nil {
				t.Fatalf("read regenerated report.md: %v", err)
			}
			gotSummary, err := os.ReadFile(filepath.Join(out, "summary.txt"))
			if err != nil {
				t.Fatalf("read regenerated summary.txt: %v", err)
			}
			if !bytes.Equal(gotMD, wantMD) {
				t.Errorf("regenerated report.md differs from committed copy")
			}
			if !bytes.Equal(gotSummary, wantSummary) {
				t.Errorf("regenerated summary.txt differs from committed copy")
			}
		})
	}
}

// containsClass reports whether want contains class.
func containsClass(want []model.HealthClass, class model.HealthClass) bool {
	for _, w := range want {
		if w == class {
			return true
		}
	}
	return false
}
