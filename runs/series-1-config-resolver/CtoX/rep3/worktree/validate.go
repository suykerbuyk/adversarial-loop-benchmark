package config

import (
	"errors"
	"fmt"
)

// ErrInvalidLogLevel is wrapped by the error Resolve returns when the
// effective LogLevel is not one of "debug", "info", "warn", "error" or "".
var ErrInvalidLogLevel = errors.New("config: invalid log level")

// validate checks the effective configuration only; values overridden by a
// higher layer are never seen here.
func validate(c Config) error {
	switch c.LogLevel {
	case "", "debug", "info", "warn", "error":
		return nil
	}
	return fmt.Errorf("%w: %q", ErrInvalidLogLevel, c.LogLevel)
}
