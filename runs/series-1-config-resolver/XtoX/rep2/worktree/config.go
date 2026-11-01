// Package config resolves layered in-memory configuration values.
package config

import (
	"errors"
	"fmt"
)

// ErrInvalidLogLevel is returned when the effective log level is unsupported.
var ErrInvalidLogLevel = errors.New("invalid log level")

// Config contains the effective configuration.
type Config struct {
	Verbose    bool
	MaxRetries int
	LogLevel   string
	TimeoutSec int
}

// layer is one precedence level, with its name used in type assertion panics.
type layer struct {
	name   string
	values map[string]any
}

// resolveField returns the value from the highest-precedence layer that
// presents key. A present value must have type T.
func resolveField[T any](layers []layer, key string) T {
	for i := len(layers) - 1; i >= 0; i-- {
		if value, present := layers[i].values[key]; present {
			typed, ok := value.(T)
			if !ok {
				panic(fmt.Sprintf("config: %s layer key %q has type %T; want %T", layers[i].name, key, value, *new(T)))
			}
			return typed
		}
	}
	var zero T
	return zero
}

// Resolve combines defaults, file, environment, and flags in ascending
// precedence order. Only the effective log level is validated.
func Resolve(defaults, file, env, flags map[string]any) (Config, error) {
	layers := []layer{
		{name: "defaults", values: defaults},
		{name: "file", values: file},
		{name: "env", values: env},
		{name: "flags", values: flags},
	}

	resolved := Config{
		Verbose:    resolveField[bool](layers, "Verbose"),
		MaxRetries: resolveField[int](layers, "MaxRetries"),
		LogLevel:   resolveField[string](layers, "LogLevel"),
		TimeoutSec: resolveField[int](layers, "TimeoutSec"),
	}

	switch resolved.LogLevel {
	case "", "debug", "info", "warn", "error":
		return resolved, nil
	default:
		return resolved, fmt.Errorf("%w: %q", ErrInvalidLogLevel, resolved.LogLevel)
	}
}
