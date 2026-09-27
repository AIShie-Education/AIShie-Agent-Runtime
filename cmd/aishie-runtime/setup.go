package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/pricing"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/redact"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store/memstore"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store/pgstore"
)

// loaded is the configuration as run and check load it.
type loaded struct {
	env    config.Env
	cfg    *config.Config
	prices *pricing.Table
	// pricesPath is the table's file, "" when there is none.
	pricesPath string
}

// load reads the environment, then the configuration (config.Load) held to
// CORE_BASE_URL_ALLOWLIST, then the price table: PRICES, else the
// runtime's prices_ref. Without a table, costs are unknown; an agent with a
// quota in dollars then cannot be held to it, and is refused, as is one
// whose model the table has no price for.
func load(env config.Env) (*loaded, error) {
	cfg, err := config.Load(env.ConfigPaths...)
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(env.CoreBaseURLAllowlist); err != nil {
		return nil, err
	}
	l := &loaded{env: env, cfg: cfg, pricesPath: env.PricesPath}
	if l.pricesPath == "" {
		l.pricesPath = cfg.PricesPath()
	}
	if l.pricesPath != "" {
		if l.prices, err = pricing.Load(l.pricesPath); err != nil {
			return nil, err
		}
	}
	if problems := usdWithoutPrices(cfg, l.prices, time.Now()); len(problems) > 0 {
		errs := make([]error, len(problems))
		for i, p := range problems {
			errs[i] = errors.New(p)
		}
		return nil, errors.Join(errs...)
	}
	return l, nil
}

// usdWithoutPrices lists the agents with a quota in dollars that no price
// can hold: there is no price table, or no row prices their model (or its
// fallback) today. A course's own settings count as the agent's.
func usdWithoutPrices(cfg *config.Config, t *pricing.Table, at time.Time) []string {
	var out []string
	for _, a := range cfg.Agents {
		views := []*config.Effective{{Agent: *a, Enabled: true}}
		courses := make([]string, 0, len(a.Courses))
		for id := range a.Courses {
			courses = append(courses, id)
		}
		slices.Sort(courses)
		for _, id := range courses {
			if e, err := a.ForCourse(id); err == nil {
				views = append(views, e)
			}
		}
		seen := map[string]bool{}
		for _, e := range views {
			if !usdApplies(cfg, e) {
				continue
			}
			models := []config.Model{e.Model}
			if e.Model.Fallback != nil {
				models = append(models, *e.Model.Fallback)
			}
			for _, m := range models {
				provider := m.EffectiveProvider()
				var p string
				switch _, ok := t.Lookup(provider, m.Model, at); {
				case t == nil:
					p = fmt.Sprintf("agent %q: it has a quota in dollars, and there is no price table (PRICES or runtime.prices_ref) to hold it to", a.ID)
				case !ok:
					p = fmt.Sprintf("agent %q: it has a quota in dollars, and the price table has no price for %s %s", a.ID, provider, m.Model)
				}
				if p != "" && !seen[p] {
					seen[p] = true
					out = append(out, p)
				}
			}
		}
	}
	return out
}

// usdApplies reports whether a quota in dollars applies to e: its own per
// agent or per asker, or its tenant's on the school's key.
func usdApplies(cfg *config.Config, e *config.Effective) bool {
	if e.Budgets.PerAgentDay.USD != nil || e.Budgets.PerAskerDay.USD != nil {
		return true
	}
	if e.Model.KeySource != config.KeySchool || e.TenantID == "" {
		return false
	}
	t, ok := cfg.Runtime.Tenants[e.TenantID]
	return ok && t.PerDay.USD != nil
}

// newLogger is the runtime's logger: JSON or text at LOG_LEVEL, every line
// passed through redaction (the built-in token and key shapes, and
// LOG_REDACT_EXTRA).
func newLogger(env config.Env, w io.Writer) (*slog.Logger, error) {
	patterns, err := env.RedactPatterns()
	if err != nil {
		return nil, err
	}
	opts := &slog.HandlerOptions{Level: env.Level()}
	var h slog.Handler
	if env.LogFormat == "text" {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}
	return slog.New(redact.NewHandler(h, patterns)), nil
}

// egressClient is the client every call out goes through: EGRESS_PROXY
// when set, else the environment's proxy (HTTPS_PROXY, NO_PROXY).
func egressClient(env config.Env) (*http.Client, error) {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("the default HTTP transport is not an *http.Transport")
	}
	tr := base.Clone()
	tr.Proxy = http.ProxyFromEnvironment
	if env.EgressProxy != "" {
		u, err := url.Parse(env.EgressProxy)
		if err != nil {
			return nil, errors.New("EGRESS_PROXY is not a URL")
		}
		tr.Proxy = http.ProxyURL(u)
	}
	return &http.Client{Transport: tr}, nil
}

// openStore opens the runtime's state: PostgreSQL at DATABASE_URL, whose
// schema must be this binary's or newer; or, when DATABASE_URL is not set,
// memory, with a loud warning.
func openStore(ctx context.Context, env config.Env, log *slog.Logger) (store.Store, string, error) {
	if env.DatabaseURL == "" {
		log.Warn("DATABASE_URL is not set: the runtime's state is kept in memory. Answers written ahead and the agents' memory " +
			"are lost when it stops, and only one worker may run. Set DATABASE_URL for anything that matters.")
		return memstore.New(), "memory", nil
	}
	cur, latest, _, err := pgstore.SchemaVersion(ctx, env.DatabaseURL)
	if err == nil && cur > latest {
		log.Warn("the database's schema is newer than this binary's: a newer release migrated it; this one works with it",
			"schema", cur, "binary", latest)
	}
	st, err := pgstore.Open(ctx, env.DatabaseURL)
	if err != nil {
		return nil, "", err
	}
	return st, "postgres", nil
}

// problemsText is err as lines, one per problem.
func problemsText(err error) string {
	if ps := config.Problems(err); len(ps) > 0 {
		lines := make([]string, len(ps))
		for i, p := range ps {
			lines[i] = "  " + p.Error()
		}
		return strings.Join(lines, "\n")
	}
	var lines []string
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range j.Unwrap() {
			lines = append(lines, "  "+e.Error())
		}
		return strings.Join(lines, "\n")
	}
	return "  " + err.Error()
}
