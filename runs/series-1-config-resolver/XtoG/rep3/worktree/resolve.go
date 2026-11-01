// Package config resolves configuration values from ordered layers.
package config

import (
	"errors"
	"fmt"
)

// ErrInvalidLogLevel indicates that the effective LogLevel is unsupported.
var ErrInvalidLogLevel = errors.New("invalid log level")

// Config is the resolved application configuration.
type Config struct {
	Verbose    bool
	MaxRetries int
	LogLevel   string
	TimeoutSec int
}

type field struct {
	key   string
	apply func(*Config, any)
}

var fields = [...]field{
	{
		key: "Verbose",
		apply: func(c *Config, value any) {
			v, ok := value.(bool)
			if !ok {
				panicType("Verbose", "bool", value)
			}
			c.Verbose = v
		},
	},
	{
		key: "MaxRetries",
		apply: func(c *Config, value any) {
			v, ok := value.(int)
			if !ok {
				panicType("MaxRetries", "int", value)
			}
			c.MaxRetries = v
		},
	},
	{
		key: "LogLevel",
		apply: func(c *Config, value any) {
			v, ok := value.(string)
			if !ok {
				panicType("LogLevel", "string", value)
			}
			c.LogLevel = v
		},
	},
	{
		key: "TimeoutSec",
		apply: func(c *Config, value any) {
			v, ok := value.(int)
			if !ok {
				panicType("TimeoutSec", "int", value)
			}
			c.TimeoutSec = v
		},
	},
}

func panicType(key, expected string, value any) {
	panic(fmt.Sprintf("config: %s must be %s (got %T)", key, expected, value))
}

// Resolve combines defaults, file, env, and flags in increasing precedence.
// Unknown keys are ignored. A present value must have the field's exact Go type.
func Resolve(defaults, file, env, flags map[string]any) (Config, error) {
	layers := [...]map[string]any{defaults, file, env, flags}
	var result Config

	for _, descriptor := range fields {
		for _, layer := range layers {
			if value, present := layer[descriptor.key]; present {
				descriptor.apply(&result, value)
			}
		}
	}

	switch result.LogLevel {
	case "", "debug", "info", "warn", "error":
		return result, nil
	default:
		return result, fmt.Errorf("LogLevel %q: %w", result.LogLevel, ErrInvalidLogLevel)
	}
}
