package secrets

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestResolve(t *testing.T) {
	dir := t.TempDir()
	write := func(p, v string) {
		t.Helper()
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("school/keys/anthropic-main", "sk-ant-file-value\n")
	write("crlf", "value-crlf\r\n\r\n")
	write("inner-space", "  spaced value \n")
	write("empty", "\n")
	write("rel/agent.key", "relative-value")
	if err := os.Mkdir(filepath.Join(dir, "adir"), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("outside-value"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	// Kubernetes mounts a secret as relative links through ..data.
	if err := os.Symlink("crlf", filepath.Join(dir, "inside-link")); err != nil {
		t.Fatal(err)
	}

	r := Resolver{
		Dir: dir,
		Getenv: env(map[string]string{
			"AISHIE_SECRET_TEN_INSTR_42_AGENTS_AGT_01J9Z_CORE_TOKEN": "ais_from_env\n",
			"CORE_TOKEN": "ais_named\n",
		}),
		BaseDir: dir,
	}
	for _, tc := range []struct {
		name, ref, want, err string
	}{
		{name: "file under SECRETS_DIR", ref: "secret://school/keys/anthropic-main", want: "sk-ant-file-value"},
		{name: "CRLF trimmed", ref: "secret://crlf", want: "value-crlf"},
		{name: "only newlines trimmed", ref: "secret://inner-space", want: "  spaced value "},
		{name: "environment fallback", ref: "secret://ten_instr_42/agents/agt_01J9Z/core_token", want: "ais_from_env"},
		{name: "symlink inside the directory", ref: "secret://inside-link", want: "value-crlf"},
		{name: "env reference", ref: "env://CORE_TOKEN", want: "ais_named"},
		{name: "relative file", ref: "file://rel/agent.key", want: "relative-value"},
		{name: "absolute file", ref: "file://" + filepath.Join(dir, "rel", "agent.key"), want: "relative-value"},
		{name: "empty file", ref: "secret://empty", err: "secret://empty: the secret is empty"},
		{name: "missing everywhere", ref: "secret://school/keys/nope", err: "not in SECRETS_DIR, and AISHIE_SECRET_SCHOOL_KEYS_NOPE is not set"},
		{name: "unset env", ref: "env://NOPE", err: "env://NOPE: NOPE is not set"},
		{name: "missing file", ref: "file://nope", err: "file://nope"},
		{name: "a directory", ref: "secret://adir", err: "is a directory"},
		{name: "symlink out of the directory", ref: "secret://escape", err: "secret://escape"},
		{name: "dot-dot", ref: "secret://../outside", err: "not '.' or '..' alone"},
		{name: "empty segment", ref: "secret://a//b", err: "each path segment"},
		{name: "no path", ref: "secret://", err: "no path"},
		{name: "bad env name", ref: "env://1ABC", err: "the name must be"},
		{name: "not a reference", ref: "sk-proj-pasted-key-value", err: "not a reference"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := r.Resolve(context.Background(), tc.ref)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("Resolve(%q) = %q, %v; want an error containing %q", tc.ref, got, err, tc.err)
				}
				for _, v := range []string{"outside-value", "sk-proj-pasted-key-value"} {
					if strings.Contains(err.Error(), v) {
						t.Fatalf("the error holds a secret: %v", err)
					}
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("Resolve(%q) = %q, %v; want %q", tc.ref, got, err, tc.want)
			}
		})
	}
}

func TestResolveWithoutDir(t *testing.T) {
	r := Resolver{Getenv: env(map[string]string{"AISHIE_SECRET_SCHOOL_KEYS_DEEPSEEK": "sk-x"})}
	if v, err := r.Resolve(context.Background(), "secret://school/keys/deepseek"); err != nil || v != "sk-x" {
		t.Fatalf("got %q, %v", v, err)
	}
	_, err := r.Resolve(context.Background(), "secret://school/keys/other")
	if err == nil || !strings.Contains(err.Error(), "SECRETS_DIR is not set") {
		t.Fatalf("got %v", err)
	}
	// A SECRETS_DIR that does not exist falls back to the environment.
	r.Dir = filepath.Join(t.TempDir(), "missing")
	if v, err := r.Resolve(context.Background(), "secret://school/keys/deepseek"); err != nil || v != "sk-x" {
		t.Fatalf("got %q, %v", v, err)
	}
}

func TestResolveOSEnvAndContext(t *testing.T) {
	t.Setenv("AISHIE_TEST_SECRET", "from-os")
	if v, err := (Resolver{}).Resolve(context.Background(), "env://AISHIE_TEST_SECRET"); err != nil || v != "from-os" {
		t.Fatalf("got %q, %v", v, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (Resolver{}).Resolve(ctx, "env://AISHIE_TEST_SECRET"); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestResolveTooLarge(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "big"), make([]byte, maxSecretBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (Resolver{Dir: dir}).Resolve(context.Background(), "secret://big"); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("got %v", err)
	}
}

func TestCheck(t *testing.T) {
	for ref, ok := range map[string]bool{
		"secret://school/keys/anthropic-main":               true,
		"secret://ten_instr_42/agents/agt_01J9Z/core_token": true,
		"env://OPENAI_API_KEY":                              true,
		"file:///run/secrets/key":                           true,
		"file://keys/local":                                 true,
		"secret://a/../b":                                   false,
		"secret://a b":                                      false,
		"env://A-B":                                         false,
		"file://":                                           false,
		"ais_k7v2m4qhx3ab_abcdef":                           false,
		"https://example.com/key":                           false,
		"":                                                  false,
	} {
		if got := IsRef(ref); got != ok {
			t.Errorf("IsRef(%q) = %v, want %v", ref, got, ok)
		}
	}
	if err := Check("ais_k7v2m4qhx3ab_abcdef"); !errors.Is(err, ErrNotReference) || strings.Contains(err.Error(), "ais_") {
		t.Fatalf("Check of a token: %v", err)
	}
}

func TestEnvName(t *testing.T) {
	for in, want := range map[string]string{
		"school/keys/anthropic-main":               "AISHIE_SECRET_SCHOOL_KEYS_ANTHROPIC_MAIN",
		"ten_instr_42/agents/agt_01J9Z/core_token": "AISHIE_SECRET_TEN_INSTR_42_AGENTS_AGT_01J9Z_CORE_TOKEN",
		"a.b": "AISHIE_SECRET_A_B",
	} {
		if got := EnvName(in); got != want {
			t.Errorf("EnvName(%q) = %q, want %q", in, got, want)
		}
	}
}

// A FIFO or a device is refused before it is opened: opening a FIFO waits
// for a writer, and a device may not end.
func TestResolveRefusesSpecialFiles(t *testing.T) {
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o600); err != nil {
		t.Skip("no FIFO here:", err)
	}
	if err := os.Symlink("/dev/zero", filepath.Join(dir, "zero")); err != nil {
		t.Fatal(err)
	}
	r := Resolver{Dir: dir, BaseDir: dir, Getenv: env(nil)}
	for _, ref := range []string{"secret://fifo", "file://fifo", "file:///dev/zero"} {
		done := make(chan error, 1)
		go func() {
			_, err := r.Resolve(context.Background(), ref)
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "not a regular file") {
				t.Errorf("%s: %v", ref, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: Resolve is still waiting", ref)
		}
	}
}

// A reference that spells a token is not repeated in an error, nor is a
// path naming one.
func TestResolveErrorsAreRedacted(t *testing.T) {
	dir := t.TempDir()
	r := Resolver{Dir: dir, BaseDir: dir, Getenv: env(nil)}
	const token = "ais_k7v2m4qhx3ab_9Jx2abcDEFghiJKLmnoPQRstuVWX"
	for _, ref := range []string{"secret://" + token, "env://" + token, "file://" + token, "file:///tmp/" + token} {
		_, err := r.Resolve(context.Background(), ref)
		if err == nil || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "[redacted]") {
			t.Errorf("%s: %v", ref, err)
		}
	}
	_, err := r.Resolve(context.Background(), "file://"+token)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the cause is kept: %v", err)
	}
}
