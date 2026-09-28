package e2e

import (
	"fmt"
	"os"
	"regexp"
	goruntime "runtime"
	"strings"
	"testing"
	"time"
)

// scenario is one end-to-end test, run in a world of its own.
type scenario struct {
	name string
	run  func(t *testing.T, w *world)
}

// scenarios are the tests of the runtime against Core, each in its own
// world, run side by side.
var scenarios = []scenario{
	{"own-agent-answers", ownAgentAnswers},
	{"tutor-keeps-askers-apart", tutorKeepsAskersApart},
	{"moved-on", movedOn},
	{"duplicates", duplicates},
	{"denied", denied},
	{"proposals", proposals},
	{"owner-writes", ownerWrites},
	{"member-writes", memberWrites},
	{"binary", theBinary},
	{"hosted-agent-from-the-registry", hostedAgentAnswers},
	{"api-takes-cores-assertion", apiTakesCoresAssertion},
	{"hosting-through-the-api", hostingThroughTheAPI},
}

// TestRuntimeAgainstCore runs every scenario against the Core under test.
// The worlds are built first, one after another: t.Setenv, which puts each
// world's tokens where the runtime's env:// references read them, cannot be
// called from a test that runs in parallel. The scenarios then run side by
// side, and last, every log they wrote is searched for tokens and keys.
func TestRuntimeAgainstCore(t *testing.T) {
	api, root := liveCore(t)
	worlds := make([]*world, len(scenarios))
	built := time.Now()
	for i, sc := range scenarios {
		worlds[i] = newWorld(t, api, root, sc.name)
	}
	t.Logf("built %d worlds in Core in %s", len(worlds), time.Since(built).Round(time.Millisecond))
	t.Run("scenarios", func(t *testing.T) {
		for i, sc := range scenarios {
			t.Run(sc.name, func(t *testing.T) {
				t.Parallel()
				sc.run(t, worlds[i])
			})
		}
	})
	t.Run("no-token-in-any-log", func(t *testing.T) { noTokenInAnyLog(t, worlds) })
}

var (
	// coreTokenRe is a Core token or invitation in any form.
	coreTokenRe = regexp.MustCompile(`ais(?:inv)?_[A-Za-z0-9_-]+`)
	// providerKeyRe is a provider key of the sk- shape, as the tests' is.
	providerKeyRe = regexp.MustCompile(`(^|[^A-Za-z0-9])sk-[A-Za-z0-9_-]{8,}`)
)

// noTokenInAnyLog is M1's "no token in any log": no log the runtime or its
// binary wrote in any scenario holds a Core token, nor the provider key,
// nor any token or key of any world, whole or its secret part. The logs
// are searched as the runtime wrote them (through redact) and also as
// they were before redaction, since redaction is a net under the rule that
// secrets are never logged in the first place (design §7). A failure names
// the log, the line and whose secret it is, never the secret.
func noTokenInAnyLog(t *testing.T, worlds []*world) {
	var secrets []secret
	for _, w := range worlds {
		secrets = append(secrets, w.secrets()...)
	}
	logs, lines := 0, 0
	for _, w := range worlds {
		for _, c := range w.captures() {
			text := c.text()
			if strings.TrimSpace(text) == "" {
				t.Errorf("%s is empty: nothing was searched", c.name)
				continue
			}
			logs++
			for n, line := range strings.Split(text, "\n") {
				lines++
				if holdsToken(line) {
					t.Errorf("%s, line %d, holds a Core token (ais_…)", c.name, n+1)
				}
				if providerKeyRe.MatchString(line) {
					t.Errorf("%s, line %d, holds a provider key (sk-…)", c.name, n+1)
				}
				for _, s := range secrets {
					for _, part := range secretParts(s.value) {
						if strings.Contains(line, part) {
							t.Errorf("%s, line %d, holds %s", c.name, n+1, s.what)
						}
					}
				}
			}
		}
	}
	if logs == 0 {
		t.Fatal("no log was written, so none could be searched")
	}
	t.Logf("searched %d logs, %d lines, for %d secrets", logs, lines, len(secrets))
}

// hintRe is a token's hint, what the API shows of one (vault.Hint): ais_,
// its public prefix, and an ellipsis.
var hintRe = regexp.MustCompile(`^ais_[a-z2-7]{12}…`)

// holdsToken reports whether line holds a Core token: an ais_… that is not
// a token's hint.
func holdsToken(line string) bool {
	for _, loc := range coreTokenRe.FindAllStringIndex(line, -1) {
		if !hintRe.MatchString(line[loc[0]:]) {
			return true
		}
	}
	return false
}

// secretParts are what of a secret must not be found: all of it, and for a
// Core token (ais_, a public prefix of 12, _, the secret) its secret part
// on its own.
func secretParts(v string) []string {
	if v == "" {
		return nil
	}
	parts := []string{v}
	const public = len("ais_") + 12 + len("_")
	if strings.HasPrefix(v, "ais_") && len(v) > public+8 {
		parts = append(parts, v[public:])
	}
	return parts
}

// TestMain runs the tests, then holds them to leaving nothing behind: once
// they pass, no goroutine of the runtime, of these tests or of an httptest
// server may still be running a few seconds later.
func TestMain(m *testing.M) {
	code := m.Run()
	if code == 0 {
		if left := leftovers(10 * time.Second); left != "" {
			_, _ = fmt.Fprintf(os.Stderr, "the tests left goroutines behind:\n\n%s\n", left)
			code = 1
		}
	}
	os.Exit(code)
}

// leftovers waits up to d for the goroutines of the runtime, of these tests
// and of httptest servers to end, and returns the stacks of those that
// have not.
func leftovers(d time.Duration) string {
	var left []string
	within(d, func() bool {
		left = ours()
		return len(left) == 0
	})
	return strings.Join(left, "\n\n")
}

// ours are the stacks of the goroutines, but this one, that run code of
// this module or an httptest server.
func ours() []string {
	buf := make([]byte, 16<<20)
	n := goruntime.Stack(buf, true)
	var out []string
	for i, g := range strings.Split(string(buf[:n]), "\n\n") {
		if i == 0 {
			continue // the goroutine asking
		}
		if strings.Contains(g, "AIShie-Agent-Runtime/") || strings.Contains(g, "net/http/httptest.") {
			out = append(out, g)
		}
	}
	return out
}

// A token's hint is not a token; a token is, whether or not a hint's shape
// begins it.
func TestHoldsToken(t *testing.T) {
	for line, want := range map[string]bool{
		`{"hint":"ais_k7v2m4qhx3ab…"}`:                                             false,
		`{"token":"ais_k7v2m4qhx3ab_9Jx2abcdefghijklmnopqrstuvwxyz0123456789ABC"}`: true,
		`ais_k7v2m4qhx3ab… then ais_other_token`:                                   true,
		`ais_K7V2M4QHX3AB…`:                                                        true,
		`nothing here`:                                                             false,
	} {
		if holdsToken(line) != want {
			t.Errorf("holdsToken(%q) = %v", line, !want)
		}
	}
}
