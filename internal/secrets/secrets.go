// Package secrets turns a reference into the secret it names (Core's
// docs/agent-runtime.md §4, §5.4). Configuration holds references only:
//
//	secret://a/b/c   the file a/b/c under SECRETS_DIR, else the environment
//	                 variable AISHIE_SECRET_A_B_C
//	env://NAME       the environment variable NAME
//	file:///abs/path a file; file://rel/path is relative to the agent's file
//
// A secret is read just before it is used and never kept longer than the
// caller keeps it. Errors name the reference, never the value.
package secrets

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/redact"
)

// Schemes a reference may have.
const (
	SchemeSecret = "secret://"
	SchemeEnv    = "env://"
	SchemeFile   = "file://"
)

// EnvPrefix begins the environment variable a secret:// reference falls
// back to.
const EnvPrefix = "AISHIE_SECRET_"

// maxSecretBytes bounds a secret file: keys and tokens are short, and a
// reference to anything larger is a mistake.
const maxSecretBytes = 64 << 10

var (
	segmentRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// ErrNotReference is a value that is not a reference at all.
var ErrNotReference = errors.New("not a reference: give secret://path, env://NAME or file://path")

// IsRef reports whether s is a well-formed reference.
func IsRef(s string) bool { return Check(s) == nil }

// Check reports whether ref is a well-formed reference, without resolving
// it. A secret:// path is one or more segments of letters, digits, '.', '_'
// and '-', none of them "." or ".."; an env:// name is a variable name; a
// file:// path is not empty. A value that is no reference at all is
// ErrNotReference, whose text does not repeat it: it may be the secret
// itself, pasted where its reference belongs.
func Check(ref string) error {
	switch {
	case strings.HasPrefix(ref, SchemeSecret):
		_, err := secretPath(ref)
		return err
	case strings.HasPrefix(ref, SchemeEnv):
		if !envNameRe.MatchString(strings.TrimPrefix(ref, SchemeEnv)) {
			return fmt.Errorf("%s: the name must be letters, digits and '_', not starting with a digit", redact.String(ref))
		}
		return nil
	case strings.HasPrefix(ref, SchemeFile):
		if strings.TrimPrefix(ref, SchemeFile) == "" {
			return fmt.Errorf("%s: no path", redact.String(ref))
		}
		return nil
	}
	return ErrNotReference
}

// secretPath is a secret:// reference's path, checked.
func secretPath(ref string) (string, error) {
	p := strings.TrimPrefix(ref, SchemeSecret)
	if p == "" {
		return "", fmt.Errorf("%s: no path", redact.String(ref))
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." || !segmentRe.MatchString(seg) {
			return "", fmt.Errorf("%s: each path segment is letters, digits, '.', '_' and '-', and not '.' or '..' alone", redact.String(ref))
		}
	}
	return p, nil
}

// EnvName is the environment variable a secret:// path falls back to:
// EnvPrefix and the path in upper case, every character but a letter or a
// digit turned to '_'. secret://school/keys/anthropic-main is
// AISHIE_SECRET_SCHOOL_KEYS_ANTHROPIC_MAIN.
func EnvName(path string) string {
	var b strings.Builder
	b.WriteString(EnvPrefix)
	for _, c := range strings.ToUpper(path) {
		if (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// Resolver resolves references.
type Resolver struct {
	// Dir is SECRETS_DIR: where secret:// paths are looked for first.
	// Empty means the environment only.
	Dir string
	// Getenv reads the environment; nil is os.Getenv.
	Getenv func(string) string
	// BaseDir is what a relative file:// path is relative to: the
	// directory of the agent's configuration file.
	BaseDir string
}

// Resolve returns the secret ref names, with trailing newlines removed.
//
// secret://a/b/c is the file Dir/a/b/c when Dir is set and the file is
// there; the file is opened within Dir, so neither the path nor a symbolic
// link can reach outside it (links within Dir must be relative, as the
// ones Kubernetes makes for a mounted secret are). Otherwise it is the environment variable
// EnvName("a/b/c"). env://NAME is the environment variable NAME.
// file:///abs is that file, and file://rel is BaseDir/rel.
//
// A secret that is missing or empty is an error, which names the reference
// and never the value.
func (r Resolver) Resolve(ctx context.Context, ref string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("secrets: %s: %w", redact.String(ref), err)
	}
	if err := Check(ref); err != nil {
		return "", fmt.Errorf("secrets: %w", err)
	}
	var (
		v   string
		err error
	)
	switch {
	case strings.HasPrefix(ref, SchemeSecret):
		v, err = r.secret(ref)
	case strings.HasPrefix(ref, SchemeEnv):
		name := strings.TrimPrefix(ref, SchemeEnv)
		v = r.getenv(name)
		if v == "" {
			err = fmt.Errorf("%s: %s is not set", redact.String(ref), redact.String(name))
		}
	default:
		v, err = readFile(r.filePath(ref))
		if err != nil {
			err = fmt.Errorf("%s: %w", redact.String(ref), redactedError{err})
		}
	}
	if err != nil {
		return "", fmt.Errorf("secrets: %w", err)
	}
	v = strings.TrimRight(v, "\r\n")
	if v == "" {
		return "", fmt.Errorf("secrets: %s: the secret is empty", redact.String(ref))
	}
	return v, nil
}

func (r Resolver) getenv(name string) string {
	if r.Getenv != nil {
		return r.Getenv(name)
	}
	return os.Getenv(name)
}

// secret reads a secret:// reference: the file under Dir if it is there,
// else the environment.
func (r Resolver) secret(ref string) (string, error) {
	p, err := secretPath(ref)
	if err != nil {
		return "", err
	}
	if r.Dir != "" {
		v, err := readInRoot(r.Dir, p)
		switch {
		case err == nil:
			return v, nil
		case !errors.Is(err, fs.ErrNotExist):
			return "", fmt.Errorf("%s: %w", redact.String(ref), redactedError{err})
		}
	}
	name := EnvName(p)
	if v := r.getenv(name); v != "" {
		return v, nil
	}
	if r.Dir != "" {
		return "", fmt.Errorf("%s: not in SECRETS_DIR, and %s is not set", redact.String(ref), redact.String(name))
	}
	return "", fmt.Errorf("%s: %s is not set, and SECRETS_DIR is not set", redact.String(ref), redact.String(name))
}

// filePath is a file:// reference's path: absolute as written, or relative
// to BaseDir.
func (r Resolver) filePath(ref string) string {
	p := strings.TrimPrefix(ref, SchemeFile)
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(r.BaseDir, p)
}

// readInRoot reads the file at slash-separated path p within dir. os.Root
// refuses a path, or a symbolic link, that leads outside dir.
func readInRoot(dir, p string) (string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	name := filepath.FromSlash(p)
	info, err := root.Stat(name)
	if err != nil {
		return "", err
	}
	if err := regular(info); err != nil {
		return "", err
	}
	f, err := root.Open(name)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	return readLimited(f)
}

func readFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if err := regular(info); err != nil {
		return "", err
	}
	f, err := os.Open(path) // #nosec G304 -- the path is the operator's configuration, a file:// reference.
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	return readLimited(f)
}

// redactedError is an error whose text is redacted, as an *fs.PathError
// naming a file after what a reference spells can need.
type redactedError struct{ err error }

func (e redactedError) Error() string { return redact.String(e.err.Error()) }
func (e redactedError) Unwrap() error { return e.err }

// regular refuses all but a regular file before it is opened: opening a
// FIFO waits for a writer, and a device may never end.
func regular(info fs.FileInfo) error {
	switch {
	case info.IsDir():
		return errors.New("is a directory")
	case !info.Mode().IsRegular():
		return errors.New("is not a regular file")
	}
	return nil
}

func readLimited(f *os.File) (string, error) {
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", errors.New("is a directory")
	}
	b, err := io.ReadAll(io.LimitReader(f, maxSecretBytes+1))
	if err != nil {
		return "", err
	}
	if len(b) > maxSecretBytes {
		return "", fmt.Errorf("larger than %d bytes", maxSecretBytes)
	}
	return string(b), nil
}
