package config

import (
	"errors"
	"fmt"
)

// ErrInvalidLogLevel is returned when the effective LogLevel is unsupported.
var ErrInvalidLogLevel = errors.New("invalid log level")

// Config contains the effective configuration after layer resolution.
type Config struct {
	Verbose    bool
	MaxRetries int
	LogLevel   string
	TimeoutSec int
}

type field struct {
	key      string
	assign   func(*Config, any)
	validate func(*Config) error
}

var fields = []field{
	{
		key: "Verbose",
		assign: func(c *Config, value any) {
			v, ok := value.(bool)
			if !ok {
				panicType("Verbose", value, "bool")
			}
			c.Verbose = v
		},
	},
	{
		key: "MaxRetries",
		assign: func(c *Config, value any) {
			v, ok := value.(int)
			if !ok {
				panicType("MaxRetries", value, "int")
			}
			c.MaxRetries = v
		},
	},
	{
		key: "LogLevel",
		assign: func(c *Config, value any) {
			v, ok := value.(string)
			if !ok {
				panicType("LogLevel", value, "string")
			}
			c.LogLevel = v
		},
		validate: func(c *Config) error {
			switch c.LogLevel {
			case "", "debug", "info", "warn", "error":
				return nil
			default:
				return fmt.Errorf("%w: %q", ErrInvalidLogLevel, c.LogLevel)
			}
		},
	},
	{
		key: "TimeoutSec",
		assign: func(c *Config, value any) {
			v, ok := value.(int)
			if !ok {
				panicType("TimeoutSec", value, "int")
			}
			c.TimeoutSec = v
		},
	},
}

type layer struct {
	name   string
	values map[string]any
}

// Resolve overlays file, environment, and flags values on defaults, in that
// order. Map membership determines whether a value is presented, so zero
// values can explicitly override lower layers.
func Resolve(defaults, file, env, flags map[string]any) (Config, error) {
	layers := []layer{
		{name: "defaults", values: defaults},
		{name: "file", values: file},
		{name: "env", values: env},
		{name: "flags", values: flags},
	}

	known := make(map[string]field, len(fields))
	for _, descriptor := range fields {
		known[descriptor.key] = descriptor
	}
	for _, current := range layers {
		for key := range current.values {
			if _, ok := known[key]; !ok {
				panic(fmt.Sprintf("unknown configuration key %q in %s layer", key, current.name))
			}
		}
	}

	var result Config
	for _, descriptor := range fields {
		// Check all presented values, including values hidden by a higher layer.
		for _, current := range layers {
			if value, present := current.values[descriptor.key]; present {
				descriptor.assign(&Config{}, value)
			}
		}
		// The reverse walk selects the highest-precedence presented value.
		for i := len(layers) - 1; i >= 0; i-- {
			if value, present := layers[i].values[descriptor.key]; present {
				descriptor.assign(&result, value)
				break
			}
		}
	}

	for _, descriptor := range fields {
		if descriptor.validate != nil {
			if err := descriptor.validate(&result); err != nil {
				return result, err
			}
		}
	}
	return result, nil
}

func panicType(key string, value any, want string) {
	panic(fmt.Sprintf("configuration key %q has type %T, want %s", key, value, want))
}
