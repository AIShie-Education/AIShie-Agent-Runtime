package rendition

import (
	"errors"
	"fmt"
	"time"
)

// Config is the renditions worker's settings (the environment's
// RENDITIONS*, docs/deploying.md). A zero field is its default.
type Config struct {
	// Mode is ModeAuto (on wherever LibreOffice converts here and Core is
	// named), ModeOn (the same, and run refuses to start where it cannot)
	// or ModeOff.
	Mode string
	// Concurrency is how many files this process converts at once, each
	// its own claim in Core.
	Concurrency int
	// Timeout bounds one file's conversion by LibreOffice.
	Timeout time.Duration
	// Lease is each claim's in Core, renewed every half of it while the
	// file is converted.
	Lease time.Duration
}

// Modes of Config.
const (
	ModeAuto = "auto"
	ModeOn   = "on"
	ModeOff  = "off"
)

// Defaults and bounds of Config.
const (
	DefaultConcurrency = 1
	MaxConcurrency     = 8
	DefaultTimeout     = 5 * time.Minute
	MinTimeout         = 5 * time.Second
	MaxTimeout         = time.Hour
	DefaultLease       = 10 * time.Minute
	// MinLease and MaxLease are Core's bounds of a claim's lease.
	MinLease = time.Minute
	MaxLease = time.Hour
)

// WithDefaults is c with its zero fields set to their defaults.
func (c Config) WithDefaults() Config {
	if c.Mode == "" {
		c.Mode = ModeAuto
	}
	if c.Concurrency <= 0 {
		c.Concurrency = DefaultConcurrency
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	if c.Lease <= 0 {
		c.Lease = DefaultLease
	}
	return c
}

// Check says what is wrong with c's values, if anything.
func (c Config) Check() error {
	var errs []error
	switch c.Mode {
	case "", ModeAuto, ModeOn, ModeOff:
	default:
		errs = append(errs, fmt.Errorf("renditions: mode %q is not auto, on or off", c.Mode))
	}
	if c.Concurrency < 0 || c.Concurrency > MaxConcurrency {
		errs = append(errs, fmt.Errorf("renditions: %d at once is not between 1 and %d", c.Concurrency, MaxConcurrency))
	}
	if c.Timeout < 0 || c.Timeout > 0 && c.Timeout < MinTimeout || c.Timeout > MaxTimeout {
		errs = append(errs, fmt.Errorf("renditions: a timeout of %s is not between %s (LibreOffice takes that to start) and %s", c.Timeout,
			MinTimeout, MaxTimeout))
	}
	if c.Lease < 0 || c.Lease > 0 && c.Lease < MinLease || c.Lease > MaxLease {
		errs = append(errs, fmt.Errorf("renditions: a lease of %s is not between %s and %s, as Core takes it", c.Lease, MinLease, MaxLease))
	}
	return errors.Join(errs...)
}
