package reporting

import (
	"encoding/json"

	"cablecheck/internal/model"
)

// RenderJSON renders the report as indented JSON with a trailing newline.
// encoding/json sorts map keys, so the output is deterministic for a given
// report value.
func RenderJSON(r *model.Report) ([]byte, error) {
	toRender := r
	if r != nil {
		copyReport := *r
		copyReport.Tests = r.Tests
		copyReport.Tests.TCPTrialSpread = tcpTrialSpread(r.Tests.TCP)
		toRender = &copyReport
	}
	b, err := json.MarshalIndent(toRender, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
