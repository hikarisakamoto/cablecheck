package app

import (
	"encoding/json"
	"reflect"
	"testing"

	"cablecheck/internal/model"
	"cablecheck/internal/protocol"
)

// TestIperf3CapsFieldParityAndRoundTrip pins the duplicate protocol and model
// capability shapes and the hand-written bridge between them.
func TestIperf3CapsFieldParityAndRoundTrip(t *testing.T) {
	modelType := reflect.TypeFor[model.Iperf3Caps]()
	protocolType := reflect.TypeFor[protocol.Iperf3Caps]()
	if modelType.NumField() != protocolType.NumField() {
		t.Fatalf("Iperf3Caps field count = model %d, protocol %d", modelType.NumField(), protocolType.NumField())
	}
	for i := range modelType.NumField() {
		modelField := modelType.Field(i)
		protocolField := protocolType.Field(i)
		if modelField.Name != protocolField.Name || modelField.Type != protocolField.Type || modelField.Tag != protocolField.Tag {
			t.Errorf("Iperf3Caps field %d differs: model %s %s %q, protocol %s %s %q",
				i, modelField.Name, modelField.Type, modelField.Tag,
				protocolField.Name, protocolField.Type, protocolField.Tag)
		}
	}

	want := model.Iperf3Caps{
		Version: "3.16", JSON: true, Reverse: false, Bidir: true,
		GetServerOutput: false, UDP: true, OneOff: false,
	}
	raw, err := json.Marshal(toProtocolIperfCaps(want))
	if err != nil {
		t.Fatalf("marshal protocol Iperf3Caps: %v", err)
	}
	var got model.Iperf3Caps
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal model Iperf3Caps: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Iperf3Caps round trip = %+v, want %+v", got, want)
	}
}
