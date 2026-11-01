// Package config computes an effective configuration by combining ordered
// in-memory layers. A higher-precedence layer overrides a lower one, field by
// field. The package performs no I/O: callers supply every layer's values.
package config

import (
	"errors"
	"fmt"
)

// Config is the effective configuration produced by Resolve.
type Config struct {
	Verbose    bool
	MaxRetries int
	LogLevel   string
	TimeoutSec int
}

// ErrInvalidLogLevel is matched (via errors.Is) by the error Resolve returns when the
// effective LogLevel is not one of "debug", "info", "warn", "error", or "".
var ErrInvalidLogLevel = errors.New("config: invalid log level")

// Resolve combines the layers in increasing precedence: defaults < file < env < flags.
//
// A key present in a layer sets that field, even when the value is a zero value
// (false, 0, ""). A key absent from a layer (or a nil layer) leaves the lower value in
// place. A field that no layer presents gets its Go zero value.
//
// Only the effective LogLevel is validated. If it is not "debug", "info", "warn",
// "error", or "", Resolve returns the zero Config and an error matching
// ErrInvalidLogLevel.
//
// Resolve panics on programmer errors, in any layer, even one that a higher layer
// overrides:
//   - a value whose dynamic type is not exactly the field's type (bool for Verbose,
//     int for MaxRetries and TimeoutSec, string for LogLevel), including a nil value;
//   - a key other than "Verbose", "MaxRetries", "LogLevel", "TimeoutSec". Unknown keys
//     are never ignored and never reported as errors, so a typo cannot silently drop
//     a setting.
//
// Resolve never modifies its arguments and is safe for concurrent use.
func Resolve(defaults, file, env, flags map[string]any) (Config, error) {
	return resolve([]layer{
		{name: "defaults", values: defaults},
		{name: "file", values: file},
		{name: "env", values: env},
		{name: "flags", values: flags},
	})
}

// layer is one named source of presented values. A nil values map means every
// field is absent from the layer.
type layer struct {
	name   string         // used only in panic messages
	values map[string]any // read-only
}

// resolve applies layers from lowest to highest precedence (slice order), so the
// last layer presenting a field wins. It then validates the effective values.
func resolve(layers []layer) (Config, error) {
	fields := fieldTable()
	var c Config
	for _, l := range layers {
		seen := 0
		for _, f := range fields {
			v, present := l.values[f.key]
			if !present {
				continue
			}
			seen++
			if !f.set(&c, v) {
				panic(fmt.Sprintf("config: layer %q: key %q: wrong type %T", l.name, f.key, v))
			}
		}
		if seen != len(l.values) {
			panic(fmt.Sprintf("config: layer %q: unknown key %q", l.name, firstUnknownKey(l.values, fields)))
		}
	}
	if err := validateLogLevel(c.LogLevel); err != nil {
		return Config{}, err
	}
	return c, nil
}

// firstUnknownKey returns the lexicographically smallest key of values that is not
// described by fields, so panic messages are deterministic. It returns "" if every
// key is known.
func firstUnknownKey(values map[string]any, fields []field) string {
	first, found := "", false
	for k := range values {
		if !hasKey(fields, k) && (!found || k < first) {
			first, found = k, true
		}
	}
	return first
}

func hasKey(fields []field, key string) bool {
	for _, f := range fields {
		if f.key == key {
			return true
		}
	}
	return false
}
