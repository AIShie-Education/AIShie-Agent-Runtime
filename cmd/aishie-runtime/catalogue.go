package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/version"
)

// maxCatalogueBytes bounds GET /v1/tools' answer; Core's is about 200 KB.
const maxCatalogueBytes = 16 << 20

// hashRe is a catalogue's hash written on its own, as a .sha256 file
// holds it.
var hashRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// cmdCatalogue is `aishie-runtime catalogue --core URL [--check FILE]
// [--write FILE]`: the hash of Core's catalogue, GET /v1/tools (no token
// needed); with --check, a failure when it is not FILE's, a catalogue or a
// hash; with --write, the catalogue saved to FILE as Core served it.
func cmdCatalogue(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("catalogue", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	base := fs.String("core", "", "Core's base URL")
	check := fs.String("check", "", "a catalogue (JSON), or its hash, to compare with")
	write := fs.String("write", "", "where to save the catalogue")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 || *base == "" {
		return usageError(stderr, "catalogue takes --core URL, and --check FILE and --write FILE")
	}
	raw, cat, err := fetchCatalogue(ctx, strings.TrimRight(*base, "/"))
	if err != nil {
		return failure(stderr, "%v", err)
	}
	_, _ = fmt.Fprintf(stdout, "%s  %d tools, %s\n", cat.Hash(), cat.Len(), *base)
	if *write != "" {
		if err := os.WriteFile(*write, raw, 0o644); err != nil { // #nosec G306 -- the catalogue is public: GET /v1/tools needs no token.
			return failure(stderr, "--write: %v", err)
		}
	}
	if *check == "" {
		return exitOK
	}
	want, err := snapshotHash(*check)
	if err != nil {
		return failure(stderr, "--check: %v", err)
	}
	if want != cat.Hash() {
		return failure(stderr, "Core's catalogue has changed: its hash is %s, and %s's is %s. "+
			"Look at what changed, check the runtime against it, and record it again with --write", cat.Hash(), *check, want)
	}
	_, _ = fmt.Fprintf(stdout, "the same as %s\n", *check)
	return exitOK
}

// fetchCatalogue reads GET /v1/tools at base as it is served, following no
// redirect.
func fetchCatalogue(ctx context.Context, base string) ([]byte, *core.Catalogue, error) {
	ctx, cancel := context.WithTimeout(ctx, core.DefaultTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/tools", nil)
	if err != nil {
		return nil, nil, fmt.Errorf("--core: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "aishie-runtime/"+version.Version)
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("GET /v1/tools: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalogueBytes+1))
	switch {
	case err != nil:
		return nil, nil, fmt.Errorf("GET /v1/tools: %w", err)
	case len(raw) > maxCatalogueBytes:
		return nil, nil, errors.New("GET /v1/tools: the answer is larger than a catalogue")
	case resp.StatusCode != http.StatusOK:
		return nil, nil, fmt.Errorf("GET /v1/tools: HTTP %d", resp.StatusCode)
	}
	cat, err := core.ParseCatalogue(raw)
	if err != nil {
		return nil, nil, err
	}
	return raw, cat, nil
}

// snapshotHash is the hash a file gives: a catalogue's, or a hash written
// on its own.
func snapshotHash(path string) (string, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- the operator names the file.
	if err != nil {
		return "", err
	}
	if s := strings.TrimSpace(string(b)); hashRe.MatchString(s) {
		return s, nil
	}
	cat, err := core.ParseCatalogue(b)
	if err != nil {
		return "", fmt.Errorf("%s is neither a catalogue nor a hash: %w", path, err)
	}
	return cat.Hash(), nil
}
