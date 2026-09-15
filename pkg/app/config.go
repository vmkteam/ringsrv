package app

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Defaults for the limits a config may leave at zero; Validate fills them in and
// says so in the boot log. A zero used to mean "no limit" — an http.Client
// without a timeout, gojq without a deadline — which is the one meaning an
// omitted limit must never have.
const (
	defaultUpstreamTimeout = 20 * time.Second
	defaultJQTimeout       = 2 * time.Second
	defaultMaxBytes        = 32768
	defaultMaxConcurrent   = 8
	defaultDBTimeout       = 15 * time.Second
	defaultDBMaxRows       = 200
	maxPort                = 65535
)

// Validate checks the instance config and fills in the limits it left at zero.
// It returns the defaults it applied, so the boot log can name the values the
// operator did not choose, and joins every finding into one error: a config
// wrong in three places should cost one restart, not three.
//
// What fails loudly a moment later is not checked here — a busy port, an
// unreachable issuer. Validate is for the mistakes that degrade instead.
func (c *Config) Validate() (applied []string, err error) {
	var errs []error

	if c.Server.Port <= 0 || c.Server.Port > maxPort {
		errs = append(errs, fmt.Errorf("Server.Port %d must be between 1 and %d", c.Server.Port, maxPort))
	}
	if c.Catalog.Path == "" {
		errs = append(errs, errors.New("Catalog.Path is required"))
	}

	errs = append(errs, c.validateOIDC()...)
	errs = append(errs, c.validateStorage()...)

	applied, limitErrs := c.applyLimitDefaults()
	errs = append(errs, limitErrs...)
	errs = append(errs, c.validateRateLimit()...)
	errs = append(errs, c.validateAPIKeys()...)

	return applied, errors.Join(errs...)
}

func (c *Config) validateOIDC() []error {
	var errs []error
	if c.OIDC.Required && c.OIDC.Issuer == "" {
		errs = append(errs, errors.New("OIDC.Required is set but OIDC.Issuer is empty"))
	}
	if c.OIDC.Issuer != "" && c.OIDC.ClientID == "" {
		errs = append(errs, errors.New("OIDC.ClientID is required when OIDC.Issuer is set"))
	}
	if err := c.audienceMismatch(); err != nil {
		errs = append(errs, err)
	}
	return errs
}

// audienceMismatch is the most common failure of this stack: the token is minted
// for one string, the resource metadata advertises another, and every call 401s
// with a valid token in hand. Nothing crashes — the service serves and refuses
// everyone — so the check is explicit.
func (c *Config) audienceMismatch() error {
	if c.OIDC.Audience != "" && c.OIDC.Audience != strings.TrimRight(c.Server.BaseURL, "/") {
		return fmt.Errorf("OIDC.Audience %q must equal Server.BaseURL %q", c.OIDC.Audience, c.Server.BaseURL)
	}
	return nil
}

func (c *Config) validateStorage() []error {
	var errs []error
	if c.Storage.MaxDiskBytes < 0 {
		errs = append(errs, fmt.Errorf("Storage.MaxDiskBytes %d must be >= 0", c.Storage.MaxDiskBytes))
	}
	// The engine is only ever asked about a worktree, and worktrees need a
	// mirror: an engine without a repos dir is a flag that does nothing.
	if c.Storage.ASTEngine && c.Storage.ReposDir == "" {
		errs = append(errs, errors.New("Storage.ASTEngine is set but Storage.ReposDir is empty"))
	}
	if c.Storage.ReposDir != "" && c.Storage.WorktreesDir == "" {
		errs = append(errs, errors.New("Storage.WorktreesDir is required when Storage.ReposDir is set"))
	}
	return errs
}

// limit is one entry of Limits: where the value lives and what it becomes when
// the config leaves it at zero.
type limit[T ~int | ~int64] struct {
	name     string
	value    *T
	fallback T
}

// applyLimits fills the zero limits in the table and refuses the negative ones.
// A negative value is not "off", it is a typo that used to switch the limit off.
func applyLimits[T ~int | ~int64](limits []limit[T]) (applied []string, errs []error) {
	for _, l := range limits {
		switch {
		case *l.value < 0:
			errs = append(errs, fmt.Errorf("%s %v must be >= 0", l.name, *l.value))
		case *l.value == 0:
			*l.value = l.fallback
			applied = append(applied, fmt.Sprintf("%s=%v", l.name, l.fallback))
		}
	}
	return applied, errs
}

// applyLimitDefaults fills the zero limits and refuses negative ones. Durations
// and counts are two tables because they are two types; %v prints both the way
// the config writes them — "20s" and "32768".
func (c *Config) applyLimitDefaults() (applied []string, errs []error) {
	l := &c.Limits
	timeouts, timeoutErrs := applyLimits([]limit[time.Duration]{
		{"Limits.UpstreamTimeout", &l.UpstreamTimeout, defaultUpstreamTimeout},
		{"Limits.JQTimeout", &l.JQTimeout, defaultJQTimeout},
		{"Limits.DBTimeout", &l.DBTimeout, defaultDBTimeout},
	})
	counts, countErrs := applyLimits([]limit[int]{
		{"Limits.MaxBytes", &l.MaxBytes, defaultMaxBytes},
		{"Limits.MaxConcurrent", &l.MaxConcurrent, defaultMaxConcurrent},
		{"Limits.DBMaxRows", &l.DBMaxRows, defaultDBMaxRows},
	})
	return append(timeouts, counts...), append(timeoutErrs, countErrs...)
}

// validateRateLimit: each limit at zero is off by design (see RateLimitConfig);
// below zero it is a mistake.
func (c *Config) validateRateLimit() []error {
	var errs []error
	r := c.RateLimit
	if r.PerUserRPM < 0 {
		errs = append(errs, fmt.Errorf("RateLimit.PerUserRPM %d must be >= 0", r.PerUserRPM))
	}
	if r.PerUserConcurrent < 0 {
		errs = append(errs, fmt.Errorf("RateLimit.PerUserConcurrent %d must be >= 0", r.PerUserConcurrent))
	}
	if r.GlobalConcurrent < 0 {
		errs = append(errs, fmt.Errorf("RateLimit.GlobalConcurrent %d must be >= 0", r.GlobalConcurrent))
	}
	if r.CostBudgetPerHour < 0 {
		errs = append(errs, fmt.Errorf("RateLimit.CostBudgetPerHour %s must be >= 0", r.CostBudgetPerHour))
	}
	return errs
}

// validateAPIKeys checks the shape of each key. A key without groups is reported
// at boot by initAPIKeys rather than refused here: it is a working credential
// that every role denies, and the operator may be mid-way through issuing it.
func (c *Config) validateAPIKeys() []error {
	var errs []error
	for i, k := range c.APIKeys {
		if k.UserID == "" {
			errs = append(errs, fmt.Errorf("ApiKeys[%d]: UserID is required", i))
		}
		if strings.TrimSpace(k.KeyHash) == "" {
			errs = append(errs, fmt.Errorf("ApiKeys[%d]: KeyHash is required", i))
		}
	}
	return errs
}
