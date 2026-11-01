// Package config resolves layered in-memory configuration values.
package config

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
)

// ErrInvalidLogLevel is returned when the effective LogLevel is unsupported.
var ErrInvalidLogLevel = errors.New("invalid log level")

// Config is the effective configuration produced by Resolve.
type Config struct {
	Verbose    bool
	MaxRetries int
	LogLevel   string
	TimeoutSec int
}

type namedLayer struct {
	name   string
	values map[string]any
}

type fieldDescriptor struct {
	set      func(cfg *Config, value any, layer string)
	validate func(Config) error
}

func field[T any](key string, ptr func(*Config) *T) fieldDescriptor {
	return fieldDescriptor{set: func(cfg *Config, value any, layer string) {
		typed, ok := value.(T)
		if !ok {
			panic(fmt.Sprintf("config: layer %q key %q: want %s, got %T", layer, key, reflect.TypeFor[T](), value))
		}
		*ptr(cfg) = typed
	}}
}

// Descriptors are read-only after initialization. They define both legal keys
// and their exact types, setters, and any field-specific validation.
var fields = map[string]fieldDescriptor{
	"Verbose":    field("Verbose", func(c *Config) *bool { return &c.Verbose }),
	"MaxRetries": field("MaxRetries", func(c *Config) *int { return &c.MaxRetries }),
	"LogLevel": {
		set: field("LogLevel", func(c *Config) *string { return &c.LogLevel }).set,
		validate: func(c Config) error {
			switch c.LogLevel {
			case "", "debug", "info", "warn", "error":
				return nil
			default:
				return fmt.Errorf("%w: %q", ErrInvalidLogLevel, c.LogLevel)
			}
		},
	},
	"TimeoutSec": field("TimeoutSec", func(c *Config) *int { return &c.TimeoutSec }),
}

// Resolve applies defaults, file, env, and flags in ascending precedence.
// Nil maps are empty layers; inputs are read but never mutated or retained.
// Unknown keys and values with the wrong Go type panic. If the effective
// LogLevel is invalid, Resolve returns a zero Config and an error matching
// ErrInvalidLogLevel.
func Resolve(defaults, file, env, flags map[string]any) (Config, error) {
	layers := []namedLayer{
		{name: "defaults", values: defaults},
		{name: "file", values: file},
		{name: "env", values: env},
		{name: "flags", values: flags},
	}
	var cfg Config
	for _, layer := range layers {
		keys := make([]string, 0, len(layer.values))
		for key := range layer.values {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			descriptor, ok := fields[key]
			if !ok {
				panic(fmt.Sprintf("config: layer %q: unknown key %q", layer.name, key))
			}
			descriptor.set(&cfg, layer.values[key], layer.name)
		}
	}
	for _, key := range sortedFieldKeys {
		if validate := fields[key].validate; validate != nil {
			if err := validate(cfg); err != nil {
				return Config{}, err
			}
		}
	}
	return cfg, nil
}

var sortedFieldKeys = func() []string {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}()
