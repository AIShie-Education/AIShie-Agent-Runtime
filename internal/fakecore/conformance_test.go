package fakecore

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

// The fake's half of the fixtures: every scenario of scenarios_test.go run
// against the fake, through the test controls, and its normalized record
// held to the one recorded from a real Core (record_test.go). This is the
// point of the fake: it must not drift from Core.

var showFake = flag.Bool("show-fake", false, "print the fake's normalized record of each scenario")

// fakeWorld is a world in the fake, seated as the recorder seats a real
// Core: Sato (instructor, the tutor's owner), Mori (a second instructor),
// Yuki and Ken (students), and Sato's course tutor.
type fakeWorld struct {
	t      *testing.T
	fc     *Core
	srv    *httptest.Server
	co     Course
	sato   Member
	mori   Member
	tutorA Actor
	tutorM Member
	agentC *mcpClient
	opener map[string]int
	author map[string]int
	people []Actor
	seats  []Member
	own    *mcpClient
	ownM   Member
}

func newFakeWorld(t *testing.T, o Options) *fakeWorld {
	t.Helper()
	fc := New(o)
	srv := httptest.NewServer(fc.Handler())
	t.Cleanup(srv.Close)
	w := &fakeWorld{t: t, fc: fc, srv: srv, opener: map[string]int{}, author: map[string]int{}}
	w.co = fc.AddCourse("CS101")
	must := func(m Member, err error) Member {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	sato := fc.AddPerson("Sato")
	w.sato = must(fc.Seat(sato.ID, w.co.ID, SeatOptions{Preset: "instructor"}))
	mori := fc.AddPerson("Mori")
	w.mori = must(fc.Seat(mori.ID, w.co.ID, SeatOptions{Preset: "instructor"}))
	for _, name := range []string{"Yuki", "Ken"} {
		p := fc.AddPerson(name)
		w.people = append(w.people, p)
		w.seats = append(w.seats, must(fc.Seat(p.ID, w.co.ID, SeatOptions{Preset: "student"})))
	}
	var err error
	if w.tutorA, err = fc.AddAgent("CS101 Tutor", sato.ID); err != nil {
		t.Fatal(err)
	}
	w.tutorM = must(fc.Seat(w.tutorA.ID, w.co.ID, SeatOptions{Preset: "course_tutor", Principal: w.sato.ID}))
	w.agentC = newMCPClient(srv.URL, w.tutorA.Token, srv.Client())
	if a, err := w.agentC.initialize(context.Background()); err != nil || a.Status != 200 {
		t.Fatalf("initialize: %v %d %s", err, a.Status, a.Body)
	}
	return w
}

func (w *fakeWorld) ok(err error) {
	w.t.Helper()
	if err != nil {
		w.t.Fatal(err)
	}
}

func (w *fakeWorld) course() string           { return w.co.ID }
func (w *fakeWorld) tutorSeat() string        { return w.tutorM.ID }
func (w *fakeWorld) studentSeat(i int) string { return w.seats[i].ID }
func (w *fakeWorld) agent() *mcpClient        { return w.agentC }
func (w *fakeWorld) base() string             { return w.srv.URL }
func (w *fakeWorld) tutorToken() string       { return w.tutorA.Token }
func (w *fakeWorld) rest() *restClient {
	return &restClient{base: w.srv.URL, token: w.tutorA.Token, hc: w.srv.Client()}
}

func (w *fakeWorld) ask(student int, body string) (string, string) {
	w.t.Helper()
	cv, m, err := w.fc.Ask(w.co.ID, w.seats[student].ID, w.tutorM.ID, body)
	w.ok(err)
	w.opener[cv.ID], w.author[m.ID] = student, student
	return cv.ID, m.ID
}

func (w *fakeWorld) followUp(conv, body string) string {
	w.t.Helper()
	m, err := w.fc.FollowUp(conv, body)
	w.ok(err)
	w.author[m.ID] = w.opener[conv]
	return m.ID
}

func (w *fakeWorld) retract(msg, reason string) {
	w.t.Helper()
	w.ok(w.fc.Retract(msg, w.seats[w.author[msg]].ID, reason))
}

func (w *fakeWorld) closeAsOpener(conv, reason string) {
	w.t.Helper()
	w.ok(w.fc.Close(conv, w.seats[w.opener[conv]].ID, reason))
}

func (w *fakeWorld) setTutorLevel(level string) {
	w.t.Helper()
	w.ok(w.fc.SetLevel(w.tutorM.ID, permConversationAnswer, level))
}

func (w *fakeWorld) pauseTutor()        { w.t.Helper(); w.ok(w.fc.PauseSeat(w.tutorM.ID)) }
func (w *fakeWorld) pauseStudent(i int) { w.t.Helper(); w.ok(w.fc.PauseSeat(w.seats[i].ID)) }

func (w *fakeWorld) removeStudent(i int) { w.t.Helper(); w.ok(w.fc.RemoveSeat(w.seats[i].ID)) }
func (w *fakeWorld) removeTutor()        { w.t.Helper(); w.ok(w.fc.RemoveSeat(w.tutorM.ID)) }
func (w *fakeWorld) archiveCourse()      { w.t.Helper(); w.ok(w.fc.ArchiveCourse(w.co.ID)) }

func (w *fakeWorld) ownAgent() *mcpClient {
	w.t.Helper()
	if w.own != nil {
		return w.own
	}
	a, err := w.fc.AddAgent("Yuki's helper", w.people[0].ID)
	w.ok(err)
	w.ownM, err = w.fc.Seat(a.ID, w.co.ID, SeatOptions{Preset: "delegate", Principal: w.seats[0].ID})
	w.ok(err)
	w.own = newMCPClient(w.srv.URL, a.Token, w.srv.Client())
	if h, err := w.own.initialize(context.Background()); err != nil || h.Status != 200 {
		w.t.Fatalf("initialize: %v %d %s", err, h.Status, h.Body)
	}
	return w.own
}

func (w *fakeWorld) ownAgentSeat() string {
	w.ownAgent()
	return w.ownM.ID
}

func (w *fakeWorld) askOwn(body string) (string, string) {
	w.t.Helper()
	w.ownAgent()
	cv, m, err := w.fc.Ask(w.co.ID, w.seats[0].ID, w.ownM.ID, body)
	w.ok(err)
	return cv.ID, m.ID
}

func (w *fakeWorld) approve(actionID string) {
	w.t.Helper()
	_, err := w.fc.Approve(actionID)
	w.ok(err)
}

func (w *fakeWorld) reject(actionID, reason string) {
	w.t.Helper()
	w.ok(w.fc.Reject(actionID, reason))
}

func (w *fakeWorld) expire(actionID string) bool {
	w.t.Helper()
	w.ok(w.fc.Expire(actionID))
	return true
}

func (w *fakeWorld) revokeTutorToken() {
	w.t.Helper()
	w.ok(w.fc.Revoke(w.tutorA.Token))
}

// fixturePath is where a scenario's record from a real Core is kept.
func fixturePath(name string) string { return filepath.Join("testdata", "fixtures", name+".json") }

func TestConformance(t *testing.T) {
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			var want any
			raw, err := os.ReadFile(fixturePath(sc.name))
			switch {
			case errors.Is(err, os.ErrNotExist):
				// Run it all the same, so that -show-fake shows what the fake
				// says; then fail.
				defer t.Errorf("no fixture for %s: record them from a real Core (make record-fixtures)", sc.name)
			case err != nil:
				t.Fatal(err)
			default:
				if err := decodeNumbers(raw, &want); err != nil {
					t.Fatalf("%s: %v", fixturePath(sc.name), err)
				}
			}
			o := Options{}
			if sc.rateLimited {
				// Core's defaults, as the fixture was recorded at; the clock
				// stands still, so the burst is all there is.
				now := time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC)
				o = Options{RatePerMinute: 600, RateBurst: 100, Now: func() time.Time { return now }}
			}
			w := newFakeWorld(t, o)
			s := &steps{}
			sc.run(t, w, s)
			got, err := s.fixture(sc.name)
			if err != nil {
				t.Fatal(err)
			}
			if *showFake {
				t.Logf("%s", indented(got))
			}
			if want == nil {
				return
			}
			if d := diff(want, got); d != "" {
				t.Errorf("the fake does not answer %s as Core did (%s):\n%s", sc.name, sc.about, d)
			}
		})
	}
}

// TestFixturesAreNormalized holds every fixture to having been normalized:
// no UUID, token or timestamp of the run it was recorded in is left in it.
func TestFixturesAreNormalized(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "fixtures", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no fixtures")
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, re := range []*regexp.Regexp{uuidRE, tokenRE} {
			if re.Match(raw) {
				t.Errorf("%s holds an id or a token of the run: %v", f, re)
			}
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}
