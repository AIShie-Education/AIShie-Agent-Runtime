package main

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/fakecore"
)

// TestCheckRenditions: check says whether PDF renditions are made here,
// and why not; with RENDITIONS=on, a runtime where they cannot be (no
// LibreOffice, no Core, a Core without renditions) does not pass, and one
// whose Core has them does, LibreOffice installed.
func TestCheckRenditions(t *testing.T) {
	examples := []string{"CONFIG", "../../examples/runtime.yaml,../../examples/agents"}
	code, out, errs := runCmd(t, env(append(examples, "RENDITIONS", "off")...), "check")
	if code != exitOK || !strings.Contains(out, "renditions: off (RENDITIONS=off)") {
		t.Errorf("RENDITIONS=off: %d\n%s%s", code, out, errs)
	}
	// A check that passes says whether LibreOffice converts here; one
	// refused for its settings prints no such line.
	noOffice := strings.Contains(out, "office: off")
	code, out, errs = runCmd(t, env(append(examples, "OFFICE_PDF", "off")...), "check")
	if code != exitOK || !strings.Contains(out, "renditions: off: no PDF renditions are made here: LibreOffice does not convert here: OFFICE_PDF=off") {
		t.Errorf("OFFICE_PDF=off: %d\n%s%s", code, out, errs)
	}
	code, out, errs = runCmd(t, env(append(examples, "OFFICE_PDF", "off", "RENDITIONS", "on")...), "check")
	if code != exitFailure || !strings.Contains(errs, "RENDITIONS=on, and PDF renditions cannot be made here: no PDF renditions are made here: "+
		"LibreOffice does not convert here") {
		t.Errorf("RENDITIONS=on, OFFICE_PDF=off: %d\n%s%s", code, out, errs)
	}
	code, out, errs = runCmd(t, env(append(examples, "RENDITIONS", "always")...), "check")
	if code != exitFailure || !strings.Contains(errs, `RENDITIONS: mode "always" is not auto, on or off`) {
		t.Errorf("RENDITIONS=always: %d\n%s%s", code, out, errs)
	}
	if noOffice {
		t.Skip("LibreOffice is not installed here: check's renditions with a Core are not tried")
	}
	for _, tc := range []struct {
		name string
		o    fakecore.Options
		code int
		want string
	}{
		{"a Core with renditions", fakecore.Options{}, exitOK, "renditions: on, 1 at once, each in 5m0s at most, with LibreOffice"},
		{"a Core without them", fakecore.Options{WithoutRenditions: true}, exitFailure, "Core has no PDF renditions"},
	} {
		srv := httptest.NewServer(fakecore.New(tc.o).Handler())
		code, out, errs := runCmd(t, env(append(examples, "RENDITIONS", "on", "CORE_BASE_URL", srv.URL)...), "check")
		srv.Close()
		if code != tc.code || !strings.Contains(out+errs, tc.want) {
			t.Errorf("%s: %d\n%s%s", tc.name, code, out, errs)
		}
	}
}
