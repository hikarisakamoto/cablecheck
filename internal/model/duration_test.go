package model

import (
	"encoding/json"
	"testing"
	"time"
)

func TestDurationJSON(t *testing.T) {
	data, err := json.Marshal(Duration(30 * time.Second))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(data) != `{"ms":30000,"text":"30s"}` {
		t.Errorf("Marshal = %s, want {\"ms\":30000,\"text\":\"30s\"}", data)
	}

	data, err = json.Marshal(Duration(90 * time.Second))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(data) != `{"ms":90000,"text":"1m30s"}` {
		t.Errorf("Marshal = %s, want {\"ms\":90000,\"text\":\"1m30s\"}", data)
	}

	unmarshal := []struct {
		in   string
		want time.Duration
	}{
		{`{"ms":30000,"text":"30s"}`, 30 * time.Second},
		{`{"ms":1500}`, 1500 * time.Millisecond}, // text optional on input
		{`30000`, 30 * time.Second},              // bare number = milliseconds
		{`250`, 250 * time.Millisecond},
		{`"30s"`, 30 * time.Second}, // Go duration string
		{`"1m30s"`, 90 * time.Second},
		{`"200ms"`, 200 * time.Millisecond},
	}
	for _, tc := range unmarshal {
		var d Duration
		if err := json.Unmarshal([]byte(tc.in), &d); err != nil {
			t.Errorf("Unmarshal(%s): %v", tc.in, err)
			continue
		}
		if d.Std() != tc.want {
			t.Errorf("Unmarshal(%s) = %v, want %v", tc.in, d.Std(), tc.want)
		}
	}

	for _, bad := range []string{`"bogus"`, `"30"`, `true`, `{"text":"30s"}`, `[1]`} {
		var d Duration
		if err := json.Unmarshal([]byte(bad), &d); err == nil {
			t.Errorf("Unmarshal(%s) = %v, want error", bad, d.Std())
		}
	}
}
