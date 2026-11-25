package core

import (
	"encoding/json"
	"testing"
)

func TestActorTimeZone(t *testing.T) {
	for _, c := range []struct {
		raw, want string
	}{
		{`{"id":"a1","time_zone":"Asia/Hong_Kong"}`, "Asia/Hong_Kong"},
		{`{"id":"a1"}`, ""},
		{`{"id":"a1","time_zone":null}`, ""},
	} {
		var a Actor
		if err := json.Unmarshal([]byte(c.raw), &a); err != nil || a.TimeZone != c.want {
			t.Errorf("%s: time zone %q, %v; want %q", c.raw, a.TimeZone, err, c.want)
		}
	}
}
