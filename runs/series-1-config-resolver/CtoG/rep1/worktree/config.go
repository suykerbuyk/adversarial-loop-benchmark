// Package config computes an effective configuration from ordered layers.
//
// Layers are supplied as map[string]any values, from lowest to highest
// precedence: defaults, file, env, flags. A key present in a layer's map sets
// the corresponding field, even when the value is a zero value (false, 0, "").
// A key absent from a layer leaves the value from lower layers in place. A
// field that no layer presents keeps its Go zero value.
//
// Supplying an unknown key, or a value whose Go type does not exactly match
// the field's type, is a programmer error and causes Resolve to panic.
package config

import (
	"errors"
	"fmt"
)

// Config is the effective, resolved configuration.
type Config struct {
	Verbose    bool
	MaxRetries int
	LogLevel   string
	TimeoutSec int
}

// ErrInvalidLogLevel is wrapped by the error Resolve returns when the
// effective LogLevel is not one of "debug", "info", "warn", "error", or "".
var ErrInvalidLogLevel = errors.New("config: invalid log level")

// layer is one named source of configuration values. A nil values map
// presents nothing.
type layer struct {
	name   string
	values map[string]any
}

// Resolve merges the four layers, given in increasing precedence order, and
// returns the effective Config. Only the effective LogLevel is validated; on
// an invalid level it returns the zero Config and an error wrapping
// ErrInvalidLogLevel. Resolve never modifies its arguments.
//
// Resolve panics if any layer contains an unknown key or a value of the wrong
// Go type, even when that value would be overridden by a higher layer.
func Resolve(defaults, file, env, flags map[string]any) (Config, error) {
	return resolve([]layer{
		{name: "defaults", values: defaults},
		{name: "file", values: file},
		{name: "env", values: env},
		{name: "flags", values: flags},
	})
}

// resolve applies layers from lowest to highest precedence and validates the
// result.
func resolve(layers []layer) (Config, error) {
	var cfg Config
	levelSrc := ""
	for _, l := range layers {
		checkKeys(l)
		// Apply in field-table order so behavior (including which panic
		// fires first) is deterministic regardless of map iteration order.
		for _, f := range fields {
			v, ok := l.values[f.key]
			if !ok {
				continue
			}
			f.set(&cfg, l.name, v)
			if f.key == logLevelKey {
				levelSrc = l.name
			}
		}
	}
	if err := validateLogLevel(cfg.LogLevel); err != nil {
		return Config{}, fmt.Errorf("%w (set by layer %q)", err, levelSrc)
	}
	return cfg, nil
}

// checkKeys panics if l contains a key that names no Config field.
func checkKeys(l layer) {
	for key := range l.values {
		if _, ok := fieldsByKey[key]; !ok {
			panic(fmt.Sprintf("config: layer %q: unknown key %q", l.name, key))
		}
	}
}
