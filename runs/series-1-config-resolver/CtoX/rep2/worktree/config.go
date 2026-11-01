// Package config computes an effective configuration from ordered layers.
//
// Four layers are combined in precedence order: defaults, file, env, flags.
// A higher layer overrides a lower one for each field it presents. A field is
// presented when its key exists in the layer's map, even if the value is a zero
// value (false, 0, ""). An absent key has no effect, so the value from lower
// layers shows through. A field that no layer presents keeps its Go zero value.
//
// The package does not read the environment, files, or the command line; the
// caller supplies every layer.
package config

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Config is the effective, resolved configuration.
type Config struct {
	Verbose    bool
	MaxRetries int
	LogLevel   string
	TimeoutSec int
}

// ErrInvalidLogLevel is returned (wrapped) by Resolve when the effective
// LogLevel is not one of "debug", "info", "warn", "error", or "".
// Match it with errors.Is.
var ErrInvalidLogLevel = errors.New("config: invalid log level")

// Resolve combines the four layers, given in precedence order from lowest
// (defaults) to highest (flags), and returns the effective Config.
//
// A nil layer presents nothing. Only the effective LogLevel is validated; an
// invalid one yields a zero Config and an error matching ErrInvalidLogLevel.
//
// Resolve panics if any layer contains an unknown key or a value whose Go type
// does not exactly match its field's type. These are programmer errors and are
// detected for every presented value, including overridden ones.
// The input maps are never modified.
func Resolve(defaults, file, env, flags map[string]any) (Config, error) {
	return resolve([]layer{
		{name: "defaults", values: defaults},
		{name: "file", values: file},
		{name: "env", values: env},
		{name: "flags", values: flags},
	})
}

// layer is one named source of values. A nil values map presents nothing.
type layer struct {
	name   string // used only in panic messages
	values map[string]any
}

// field describes how one map key is type-checked and stored into a Config.
type field struct {
	key   string
	apply func(c *Config, layerName string, v any)
}

// newField builds a field whose type check and assignment share the type T,
// so the two can never disagree.
func newField[T any](key string, set func(*Config, T)) field {
	return field{key: key, apply: func(c *Config, layerName string, v any) {
		t, ok := v.(T)
		if !ok {
			panic(fmt.Sprintf("config: layer %q: key %q has type %T, want %T",
				layerName, key, v, *new(T)))
		}
		set(c, t)
	}}
}

// fields lists every configurable field. Adding a field to Config requires
// exactly one new entry here; a test keeps the two in sync.
var fields = []field{
	newField("Verbose", func(c *Config, v bool) { c.Verbose = v }),
	newField("MaxRetries", func(c *Config, v int) { c.MaxRetries = v }),
	newField("LogLevel", func(c *Config, v string) { c.LogLevel = v }),
	newField("TimeoutSec", func(c *Config, v int) { c.TimeoutSec = v }),
}

// fieldByKey indexes fields by key to detect unknown keys.
var fieldByKey = func() map[string]field {
	m := make(map[string]field, len(fields))
	for _, f := range fields {
		if _, dup := m[f.key]; dup {
			panic("config: duplicate field key " + f.key)
		}
		m[f.key] = f
	}
	return m
}()

// resolve merges layers ordered from lowest to highest precedence, then
// validates the effective result.
func resolve(layers []layer) (Config, error) {
	var c Config // fields no layer presents keep their zero value
	for _, l := range layers {
		checkUnknownKeys(l)
		for _, f := range fields {
			// Presence is decided by the comma-ok form only, never by
			// comparing against a zero value.
			if v, ok := l.values[f.key]; ok {
				f.apply(&c, l.name, v)
			}
		}
	}
	if !validLogLevel(c.LogLevel) {
		return Config{}, fmt.Errorf("%w: %q", ErrInvalidLogLevel, c.LogLevel)
	}
	return c, nil
}

// checkUnknownKeys panics if l contains any key not in the field table.
// Unknown keys are sorted so the message is deterministic.
func checkUnknownKeys(l layer) {
	var unknown []string
	for k := range l.values {
		if _, ok := fieldByKey[k]; !ok {
			unknown = append(unknown, fmt.Sprintf("%q", k))
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		panic(fmt.Sprintf("config: layer %q: unknown key(s) %s",
			l.name, strings.Join(unknown, ", ")))
	}
}

// validLogLevel reports whether s is a legal effective log level. The
// comparison is exact: case-sensitive and without trimming.
func validLogLevel(s string) bool {
	switch s {
	case "debug", "info", "warn", "error", "":
		return true
	}
	return false
}
