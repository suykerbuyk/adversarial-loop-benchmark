// Package config resolves an effective configuration from ordered in-memory layers.
// Lower layers are overridden only where a higher layer presents the same field.
package config

import (
	"errors"
	"fmt"
)

// ErrInvalidLogLevel is returned when the effective LogLevel is not one of
// "debug", "info", "warn", "error", or "" (an explicit empty level).
var ErrInvalidLogLevel = errors.New("config: invalid log level; legal values are debug, info, warn, error, and empty")

// Config is the effective configuration after layer merge and validation.
type Config struct {
	Verbose    bool
	MaxRetries int
	LogLevel   string
	TimeoutSec int
}

type fieldKind int

const (
	kindBool fieldKind = iota
	kindInt
	kindString
)

type fieldDesc struct {
	key  string
	kind fieldKind
	set  func(*Config, any)
}

// fields is the registration list. Adding a field is one descriptor and one Config field.
var fields = []fieldDesc{
	{key: "Verbose", kind: kindBool, set: func(c *Config, v any) { c.Verbose = v.(bool) }},
	{key: "MaxRetries", kind: kindInt, set: func(c *Config, v any) { c.MaxRetries = v.(int) }},
	{key: "LogLevel", kind: kindString, set: func(c *Config, v any) { c.LogLevel = v.(string) }},
	{key: "TimeoutSec", kind: kindInt, set: func(c *Config, v any) { c.TimeoutSec = v.(int) }},
}

func lookup(key string) fieldDesc {
	for _, d := range fields {
		if d.key == key {
			return d
		}
	}
	panic(fmt.Sprintf("config: unknown key %q", key))
}

func assertType(key string, kind fieldKind, value any) {
	switch kind {
	case kindBool:
		if _, ok := value.(bool); !ok {
			panic(fmt.Sprintf("config: key %q: want bool, got %T", key, value))
		}
	case kindInt:
		if _, ok := value.(int); !ok {
			panic(fmt.Sprintf("config: key %q: want int, got %T", key, value))
		}
	case kindString:
		if _, ok := value.(string); !ok {
			panic(fmt.Sprintf("config: key %q: want string, got %T", key, value))
		}
	default:
		panic(fmt.Sprintf("config: key %q: unknown field kind", key))
	}
}

func validateLogLevel(level string) error {
	switch level {
	case "debug", "info", "warn", "error", "":
		return nil
	default:
		return fmt.Errorf("config: invalid log level %q: %w", level, ErrInvalidLogLevel)
	}
}

// Resolve merges defaults, file, env, and flags from lowest to highest precedence.
// A key present in a layer sets that field, including zero values (false, 0, "").
// A key absent from a layer leaves the lower value unchanged. Nil maps are absent layers.
// Only the effective LogLevel is validated. A present key outside the legal set, or a
// present value of the wrong Go type, is a programmer error and panics.
func Resolve(defaults, file, env, flags map[string]any) (Config, error) {
	cfg := Config{}
	layers := []map[string]any{defaults, file, env, flags}
	for _, layer := range layers {
		if layer == nil {
			continue
		}
		for key, value := range layer {
			desc := lookup(key)
			assertType(desc.key, desc.kind, value)
			desc.set(&cfg, value)
		}
	}
	if err := validateLogLevel(cfg.LogLevel); err != nil {
		return cfg, err
	}
	return cfg, nil
}
