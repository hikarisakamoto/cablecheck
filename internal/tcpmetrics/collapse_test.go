package tcpmetrics

import (
	"math"
	"reflect"
	"testing"

	"cablecheck/internal/model"
)

func TestCollapseEvents(t *testing.T) {
	tests := []struct {
		name string
		rows []Sample
		want []model.TCPCollapseEvent
	}{
		{name: "empty", want: []model.TCPCollapseEvent{}},
		{
			name: "first interval excluded",
			rows: []Sample{{StartSec: 0, BitsPerSecond: 1}, {StartSec: 1, BitsPerSecond: 100}, {StartSec: 2, BitsPerSecond: 100}},
			want: []model.TCPCollapseEvent{},
		},
		{
			name: "exactly half is not below boundary",
			rows: []Sample{{BitsPerSecond: 100}, {StartSec: 1, BitsPerSecond: 50}, {StartSec: 2, BitsPerSecond: 100}, {StartSec: 3, BitsPerSecond: 100}},
			want: []model.TCPCollapseEvent{},
		},
		{
			name: "immediately below half collapses",
			rows: []Sample{{BitsPerSecond: 100}, {StartSec: 1, BitsPerSecond: 49}, {StartSec: 2, BitsPerSecond: 100}, {StartSec: 3, BitsPerSecond: 100}},
			want: []model.TCPCollapseEvent{{StartSec: 1, Len: 1, MinBps: 49}},
		},
		{
			name: "consecutive and separated runs",
			rows: []Sample{
				{BitsPerSecond: 1},
				{StartSec: 1, BitsPerSecond: 100},
				{StartSec: 2, BitsPerSecond: 30},
				{StartSec: 3, BitsPerSecond: 20},
				{StartSec: 4, BitsPerSecond: 100},
				{StartSec: 5, BitsPerSecond: 10},
				{StartSec: 6, BitsPerSecond: 100},
			},
			want: []model.TCPCollapseEvent{
				{StartSec: 2, Len: 2, MinBps: 20},
				{StartSec: 5, Len: 1, MinBps: 10},
			},
		},
		{
			name: "even median uses middle mean",
			rows: []Sample{{BitsPerSecond: 1}, {StartSec: 1, BitsPerSecond: 39}, {StartSec: 2, BitsPerSecond: 80}, {StartSec: 3, BitsPerSecond: 100}, {StartSec: 4, BitsPerSecond: 120}},
			want: []model.TCPCollapseEvent{{StartSec: 1, Len: 1, MinBps: 39}},
		},
		{
			name: "nonpositive median has no evidence",
			rows: []Sample{{BitsPerSecond: 100}, {BitsPerSecond: 0}, {BitsPerSecond: 0}},
			want: []model.TCPCollapseEvent{},
		},
		{
			name: "even median does not overflow",
			rows: []Sample{{BitsPerSecond: 1}, {BitsPerSecond: math.MaxFloat64}, {BitsPerSecond: math.MaxFloat64}},
			want: []model.TCPCollapseEvent{},
		},
		{
			name: "invalid negative sample produces no evidence",
			rows: []Sample{{BitsPerSecond: 1}, {BitsPerSecond: -1}, {BitsPerSecond: 100}},
			want: []model.TCPCollapseEvent{},
		},
		{
			name: "invalid nonfinite sample produces no evidence",
			rows: []Sample{{BitsPerSecond: 1}, {BitsPerSecond: math.NaN()}, {BitsPerSecond: 100}},
			want: []model.TCPCollapseEvent{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CollapseEvents(tc.rows)
			if got == nil {
				t.Fatal("CollapseEvents returned nil; want completed empty analysis")
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("CollapseEvents() = %+v, want %+v", got, tc.want)
			}
		})
	}
}
