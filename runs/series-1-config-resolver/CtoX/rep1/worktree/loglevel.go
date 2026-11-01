package config

import "fmt"

// validateLogLevel returns nil if s is a legal effective LogLevel, and otherwise an
// error wrapping ErrInvalidLogLevel. Matching is exact and case-sensitive. A switch
// keeps the legal set immutable by construction: there is no package state to write to.
func validateLogLevel(s string) error {
	switch s {
	case "debug", "info", "warn", "error", "":
		return nil
	}
	return fmt.Errorf("%w %q (want one of \"debug\", \"info\", \"warn\", \"error\", or \"\")",
		ErrInvalidLogLevel, s)
}
