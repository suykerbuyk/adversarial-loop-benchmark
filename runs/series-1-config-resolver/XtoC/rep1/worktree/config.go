// Package config resolves layered in-memory configuration values.
package config

import (
	"errors"
	"fmt"
	"reflect"
)

// Config is the effective configuration produced by Resolve.
type Config struct {
	Verbose    bool
	MaxRetries int
	LogLevel   string
	TimeoutSec int
}

// ErrInvalidLogLevel marks an unsupported effective LogLevel.
var ErrInvalidLogLevel = errors.New("invalid log level")

type field struct {
	key    string
	typeOf reflect.Type
	apply  func(*Config, any)
}

func fieldOf[T any](key string, set func(*Config, T)) field {
	return field{
		key:    key,
		typeOf: reflect.TypeOf((*T)(nil)).Elem(),
		apply:  func(c *Config, value any) { set(c, value.(T)) },
	}
}

var fields = []field{
	fieldOf("Verbose", func(c *Config, value bool) { c.Verbose = value }),
	fieldOf("MaxRetries", func(c *Config, value int) { c.MaxRetries = value }),
	fieldOf("LogLevel", func(c *Config, value string) { c.LogLevel = value }),
	fieldOf("TimeoutSec", func(c *Config, value int) { c.TimeoutSec = value }),
}

var fieldsByKey = func() map[string]field {
	result := make(map[string]field, len(fields))
	for _, descriptor := range fields {
		if _, exists := result[descriptor.key]; exists {
			panic(fmt.Sprintf("duplicate configuration field key %q", descriptor.key))
		}
		result[descriptor.key] = descriptor
	}
	return result
}()

type layer struct {
	name   string
	values map[string]any
}

// Resolve applies layers in ascending precedence: defaults, file, env, flags.
// It validates every presented key and type before resolution. If the effective
// LogLevel is invalid, Resolve returns the resolved Config together with an
// error wrapping ErrInvalidLogLevel.
func Resolve(defaults, file, env, flags map[string]any) (Config, error) {
	layers := [...]layer{
		{name: "defaults", values: defaults},
		{name: "file", values: file},
		{name: "env", values: env},
		{name: "flags", values: flags},
	}

	for _, current := range layers {
		for key, value := range current.values {
			descriptor, ok := fieldsByKey[key]
			if !ok {
				panic(fmt.Sprintf("unknown configuration key %q in %s layer", key, current.name))
			}
			if actual := reflect.TypeOf(value); actual != descriptor.typeOf {
				panic(fmt.Sprintf("invalid type for %s layer key %q: expected %s, got %T", current.name, key, descriptor.typeOf, value))
			}
		}
	}

	var result Config
	for _, descriptor := range fields {
		for i := len(layers) - 1; i >= 0; i-- {
			if value, present := layers[i].values[descriptor.key]; present {
				descriptor.apply(&result, value)
				break
			}
		}
	}

	switch result.LogLevel {
	case "", "debug", "info", "warn", "error":
		return result, nil
	default:
		return result, fmt.Errorf("%w: %q", ErrInvalidLogLevel, result.LogLevel)
	}
}
