// Package config resolves configuration values from ordered in-memory layers.
package config

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
)

// ErrInvalidLogLevel is returned when the effective log level is unsupported.
var ErrInvalidLogLevel = errors.New("config: invalid log level")

// ErrUnsupportedKey appears only in recovered panic values for unsupported keys.
var ErrUnsupportedKey = errors.New("config: unsupported key")

// ErrWrongType appears only in recovered panic values for wrongly typed values.
var ErrWrongType = errors.New("config: wrong value type")

// Config contains the effective configuration.
type Config struct {
	Verbose    bool
	MaxRetries int
	LogLevel   string
	TimeoutSec int
}

type fieldDescriptor struct {
	key      string
	apply    func(*Config, any, string)
	validate func(Config) error
}

func field[T any](key string, dst func(*Config) *T, validate func(T) error) fieldDescriptor {
	expected := reflect.TypeOf((*T)(nil)).Elem().String()
	descriptor := fieldDescriptor{
		key: key,
		apply: func(c *Config, value any, layer string) {
			typed, ok := value.(T)
			if !ok {
				panic(fmt.Errorf("%w: key %q in layer %q: expected %s, got %T", ErrWrongType, key, layer, expected, value))
			}
			*dst(c) = typed
		},
	}
	if validate != nil {
		descriptor.validate = func(c Config) error { return validate(*dst(&c)) }
	}
	return descriptor
}

var fields = []fieldDescriptor{
	field("Verbose", func(c *Config) *bool { return &c.Verbose }, nil),
	field("MaxRetries", func(c *Config) *int { return &c.MaxRetries }, nil),
	field("LogLevel", func(c *Config) *string { return &c.LogLevel }, validateLogLevel),
	field("TimeoutSec", func(c *Config) *int { return &c.TimeoutSec }, nil),
}

var knownKeys = func() map[string]struct{} {
	known := make(map[string]struct{}, len(fields))
	for _, descriptor := range fields {
		known[descriptor.key] = struct{}{}
	}
	return known
}()

func validateLogLevel(level string) error {
	switch level {
	case "", "debug", "info", "warn", "error":
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrInvalidLogLevel, level)
	}
}

// Resolve applies defaults, file, env, and flags in increasing precedence.
// It reads but never mutates or retains the input maps. A wrong-typed value or
// unsupported key panics with a diagnostic naming the key and layer. On an
// invalid effective log level it returns Config{} and a wrapped
// ErrInvalidLogLevel.
func Resolve(defaults, file, env, flags map[string]any) (Config, error) {
	return resolve([]namedLayer{{"defaults", defaults}, {"file", file}, {"env", env}, {"flags", flags}})
}

type namedLayer struct {
	name string
	data map[string]any
}

func resolve(layers []namedLayer) (Config, error) {
	var resolved Config
	var logLevelLayer string
	for _, layer := range layers {
		var unknown []string
		for key := range layer.data {
			if _, ok := knownKeys[key]; !ok {
				unknown = append(unknown, key)
			}
		}
		if len(unknown) != 0 {
			sort.Strings(unknown)
			panic(fmt.Errorf("%w %q in layer %q", ErrUnsupportedKey, unknown[0], layer.name))
		}
		for _, descriptor := range fields {
			if value, present := layer.data[descriptor.key]; present {
				descriptor.apply(&resolved, value, layer.name)
				if descriptor.key == "LogLevel" {
					logLevelLayer = layer.name
				}
			}
		}
	}
	for _, descriptor := range fields {
		if descriptor.validate != nil {
			if err := descriptor.validate(resolved); err != nil {
				return Config{}, fmt.Errorf("%w (from layer %q)", err, logLevelLayer)
			}
		}
	}
	return resolved, nil
}
