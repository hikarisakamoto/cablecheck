package reporting

import (
	"encoding/json"

	"cablecheck/internal/model"
)

// RenderJSON renders the report as indented JSON with a trailing newline.
// encoding/json sorts map keys, so the output is deterministic for a given
// report value.
func RenderJSON(r *model.Report) ([]byte, error) {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
