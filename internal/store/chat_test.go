package store

import (
	"encoding/json"
	"testing"
)

func TestChatResultSourcesStatedEmptySurvivesWriteAhead(t *testing.T) {
	stated := true
	original := ChatResult{Body: "answer", SourcesStated: &stated, Sources: []ChatSource{}}
	roundTrip, err := ParseChatResult(original.JSON())
	if err != nil {
		t.Fatal(err)
	}
	if roundTrip == nil || roundTrip.Sources == nil || len(roundTrip.Sources) != 0 {
		t.Fatalf("round trip = %+v", roundTrip)
	}
	encoded, err := json.Marshal(roundTrip.Sources)
	if err != nil || string(encoded) != "[]" {
		t.Fatalf("sources encode as %s, %v", encoded, err)
	}

	// Rows written by an older binary omitted an empty sources array.
	legacy, err := ParseChatResult(json.RawMessage(`{"body":"answer","sources_stated":true}`))
	if err != nil || legacy.Sources == nil {
		t.Fatalf("legacy result = %+v, %v", legacy, err)
	}
}
