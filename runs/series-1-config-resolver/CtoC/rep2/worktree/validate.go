package config

import "fmt"

// validLogLevels is the set of legal effective LogLevel values. Matching is
// exact and case-sensitive; "" is an explicit empty level.
var validLogLevels = map[string]struct{}{
	"debug": {},
	"info":  {},
	"warn":  {},
	"error": {},
	"":      {},
}

// validate checks the effective (resolved) configuration. Only LogLevel is
// constrained; overridden layer values never reach this point.
func validate(c Config) error {
	if _, ok := validLogLevels[c.LogLevel]; !ok {
		return fmt.Errorf(`config: %w %q (want one of "debug", "info", "warn", "error", or "")`,
			ErrInvalidLogLevel, c.LogLevel)
	}
	return nil
}
