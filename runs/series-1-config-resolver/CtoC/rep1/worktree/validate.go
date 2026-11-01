package config

import "fmt"

// validLogLevels is the set of legal effective LogLevel values. The match is
// exact and case-sensitive. The empty string is an explicit, legal level.
var validLogLevels = map[string]struct{}{
	"debug": {},
	"info":  {},
	"warn":  {},
	"error": {},
	"":      {},
}

// validate checks the resolved configuration. Only LogLevel is validated.
func validate(cfg Config) error {
	if _, ok := validLogLevels[cfg.LogLevel]; !ok {
		return fmt.Errorf("%w: %q", ErrInvalidLogLevel, cfg.LogLevel)
	}
	return nil
}
