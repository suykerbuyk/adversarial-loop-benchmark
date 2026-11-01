// Package config computes an effective configuration from ordered layers.
//
// Layers are applied in ascending precedence: defaults, file, env, flags.
// A layer is a map[string]any. A key present in the map sets the field, even
// when its value is a zero value (false, 0, ""). A key absent from the map
// leaves the value from lower layers in place. A field that no layer presents
// keeps its Go zero value.
//
// The package resolves in-memory data only. It never reads the environment,
// files, or the command line. The layer names describe precedence only.
package config

import (
	"errors"
	"fmt"
	"slices"
)

// Config is the effective configuration produced by Resolve.
type Config struct {
	Verbose    bool
	MaxRetries int
	LogLevel   string
	TimeoutSec int
}

// ErrInvalidLogLevel is reported (wrapped) by Resolve when the effective
// LogLevel is not one of "debug", "info", "warn", "error", or "".
// Match it with errors.Is.
var ErrInvalidLogLevel = errors.New("config: invalid log level")

// Resolve merges the four layers, given in ascending precedence order, into
// an effective Config. A nil layer presents no fields.
//
// Only the effective LogLevel is validated; a LogLevel that a higher layer
// overrides is never checked. If validation fails, Resolve returns a zero
// Config and an error satisfying errors.Is(err, ErrInvalidLogLevel). Callers
// must not rely on the Config when err != nil.
//
// Resolve panics if any layer, overridden or not, contains a key that is not a
// Config field name, or a present value whose dynamic type is not exactly the
// field's Go type (for example int64 or float64 for an int field, or nil).
// These are programmer errors, not data errors. Resolve never modifies or
// retains the input maps.
func Resolve(defaults, file, env, flags map[string]any) (Config, error) {
	return resolve([]layer{
		{name: "defaults", values: defaults},
		{name: "file", values: file},
		{name: "env", values: env},
		{name: "flags", values: flags},
	})
}

// layer is one named source of values. The name is used only in panic messages.
type layer struct {
	name   string
	values map[string]any
}

// field ties a layer key to its Config struct field and Go type.
type field struct {
	key      string
	wantType string
	// set assigns v to the field in c. It reports false, leaving c unchanged,
	// if v does not have the field's exact dynamic type.
	set func(c *Config, v any) bool
}

// bind builds a field row whose type check and assignment are tied to the
// struct field at compile time, so key, type, and field cannot drift apart.
func bind[T any](key string, ptr func(*Config) *T) field {
	var zero T
	return field{
		key:      key,
		wantType: fmt.Sprintf("%T", zero),
		set: func(c *Config, v any) bool {
			t, ok := v.(T)
			if ok {
				*ptr(c) = t
			}
			return ok
		},
	}
}

// fields lists every Config field. To add a field, add a struct field above
// and one row here.
var fields = []field{
	bind("Verbose", func(c *Config) *bool { return &c.Verbose }),
	bind("MaxRetries", func(c *Config) *int { return &c.MaxRetries }),
	bind("LogLevel", func(c *Config) *string { return &c.LogLevel }),
	bind("TimeoutSec", func(c *Config) *int { return &c.TimeoutSec }),
}

// knownKeys is derived from fields so it cannot diverge from the table.
var knownKeys = func() map[string]struct{} {
	m := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		m[f.key] = struct{}{}
	}
	return m
}()

// resolve applies layers in ascending precedence and validates the result.
// Iteration is over slices, never maps, so results and panics are deterministic.
func resolve(layers []layer) (Config, error) {
	var cfg Config // fields no layer presents keep their Go zero value
	for _, l := range layers {
		checkKeys(l)
		for _, f := range fields {
			// Presence is decided by comma-ok, never by comparing with the
			// zero value: a presented false, 0, or "" must override.
			v, present := l.values[f.key]
			if !present {
				continue
			}
			if !f.set(&cfg, v) {
				panic(fmt.Sprintf("config: layer %q: key %q: want %s, got %T",
					l.name, f.key, f.wantType, v))
			}
		}
	}
	if err := validateLogLevel(cfg.LogLevel); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// checkKeys panics if l contains a key that is not a Config field. A
// misspelled key would otherwise be silently ignored.
func checkKeys(l layer) {
	var unknown []string
	for k := range l.values {
		if _, ok := knownKeys[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		panic(fmt.Sprintf("config: layer %q: unknown key(s) %q", l.name, unknown))
	}
}

// validateLogLevel reports whether level is a legal effective log level.
// The comparison is exact and case-sensitive.
func validateLogLevel(level string) error {
	switch level {
	case "", "debug", "info", "warn", "error":
		return nil
	}
	return fmt.Errorf(`%w: %q (want one of "debug", "info", "warn", "error", or "")`,
		ErrInvalidLogLevel, level)
}
