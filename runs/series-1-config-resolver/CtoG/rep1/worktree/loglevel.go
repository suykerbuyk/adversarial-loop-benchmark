package config

import "fmt"

// validLogLevels is the exact, case-sensitive set of legal effective levels.
// The empty string is an explicit "no level" and is legal.
var validLogLevels = map[string]struct{}{
	"":      {},
	"debug": {},
	"info":  {},
	"warn":  {},
	"error": {},
}

// validateLogLevel returns an error wrapping ErrInvalidLogLevel if level is
// not a legal log level.
func validateLogLevel(level string) error {
	if _, ok := validLogLevels[level]; !ok {
		return fmt.Errorf("%w %q", ErrInvalidLogLevel, level)
	}
	return nil
}
