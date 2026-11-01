// Package config computes an effective configuration from ordered layers.
//
// Layers are applied from lowest to highest precedence: defaults, file, env,
// flags. Each layer is a map[string]any. A key present in a layer sets the
// corresponding field, even when the value is a Go zero value (false, 0, "").
// A key absent from a layer leaves the value from lower layers unchanged.
// A field that no layer presents keeps its Go zero value.
//
// Only the effective (resolved) LogLevel is validated. Values of the wrong Go
// type and unknown keys are programmer errors and cause a panic.
package config

import "errors"

// Config is the effective configuration produced by Resolve.
type Config struct {
	Verbose    bool
	MaxRetries int
	LogLevel   string
	TimeoutSec int
}

// ErrInvalidLogLevel is returned (wrapped) by Resolve when the effective
// LogLevel is not one of "debug", "info", "warn", "error", or "".
var ErrInvalidLogLevel = errors.New("config: invalid log level")

// Resolve combines the four layers in precedence order (defaults lowest,
// flags highest) and returns the effective Config.
//
// A nil layer presents no fields. If the effective LogLevel is invalid,
// Resolve returns the zero Config and an error for which
// errors.Is(err, ErrInvalidLogLevel) reports true.
//
// Resolve panics if any present value in any layer has the wrong Go type for
// its key, or if a layer contains a key that is not a Config field name.
// Input maps are only read.
func Resolve(defaults, file, env, flags map[string]any) (Config, error) {
	return resolve(defaults, file, env, flags)
}

// resolve applies layers from lowest to highest precedence and validates the
// result.
func resolve(layers ...map[string]any) (Config, error) {
	var cfg Config
	for _, layer := range layers {
		checkKeys(layer)
		for _, f := range fields {
			if v, ok := layer[f.name]; ok {
				f.apply(&cfg, v)
			}
		}
	}
	if err := validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}
