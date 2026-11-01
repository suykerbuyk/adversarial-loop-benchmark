// Package config computes an effective configuration by combining ordered
// layers of in-memory values. A higher-precedence layer overrides a lower one.
//
// The package performs no I/O: it never reads the environment, files, or the
// command line. Layer names (defaults, file, env, flags) denote precedence only.
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

// ErrInvalidLogLevel is wrapped by the error Resolve returns when the
// effective LogLevel is not one of "debug", "info", "warn", "error", or "".
var ErrInvalidLogLevel = errors.New("invalid log level")

// layer is one named source of values. The name is used only in panic messages.
type layer struct {
	name   string
	values map[string]any
}

// Resolve combines the layers in increasing precedence order
// (defaults < file < env < flags) and returns the effective Config.
//
// A key present in a layer sets the corresponding field, even when its value
// is a zero value (false, 0, ""). An absent key has no effect, so the value
// from a lower layer shows through. A field no layer presents keeps its Go
// zero value. A nil map is an empty layer.
//
// Only the effective LogLevel is validated; if it is invalid, Resolve returns
// the zero Config and an error wrapping ErrInvalidLogLevel.
//
// Resolve panics if any layer contains a key that is not a Config field name,
// or a value whose dynamic type is not exactly the field's Go type (including
// nil, int64, float64, and named types such as `type myInt int`). These are
// programmer errors. Every layer is type-checked, even when overridden.
//
// Resolve never mutates or retains the input maps and is safe for concurrent use.
func Resolve(defaults, file, env, flags map[string]any) (Config, error) {
	return resolve([]layer{
		{"defaults", defaults},
		{"file", file},
		{"env", env},
		{"flags", flags},
	})
}

// resolve applies layers ordered lowest to highest precedence onto a zero
// Config, then validates the result.
func resolve(layers []layer) (Config, error) {
	var c Config
	for _, l := range layers {
		checkKnownKeys(l)
		for _, f := range fields {
			v, present := l.values[f.key]
			if !present {
				continue
			}
			if !f.set(&c, v) {
				panic(fmt.Sprintf("config: layer %q: key %q: value %#v has type %T, want %s",
					l.name, f.key, v, v, f.typeName))
			}
		}
	}
	if err := validate(c); err != nil {
		return Config{}, err
	}
	return c, nil
}

// checkKnownKeys panics if l contains a key that names no Config field.
func checkKnownKeys(l layer) {
	for k := range l.values {
		if _, ok := fieldIndex[k]; !ok {
			panic(fmt.Sprintf("config: layer %q: unknown key %q", l.name, k))
		}
	}
}
