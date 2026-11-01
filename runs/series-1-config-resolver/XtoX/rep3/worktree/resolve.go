// Package config resolves sparse configuration layers into a typed Config.
package config

import (
	"errors"
	"fmt"
)

// ErrInvalidLogLevel is returned when the effective LogLevel is unsupported.
var ErrInvalidLogLevel = errors.New("invalid log level")

// Config contains the effective configuration values.
type Config struct {
	Verbose    bool
	MaxRetries int
	LogLevel   string
	TimeoutSec int
}

// Resolve combines defaults, file, env, and flags in increasing precedence.
// A present field replaces the prior value even when it is its type's zero value.
// A present field with a value of the wrong type panics.
func Resolve(defaults, file, env, flags map[string]any) (Config, error) {
	cfg := Config{}
	layers := []map[string]any{defaults, file, env, flags}

	for _, layer := range layers {
		if value, present := layer["Verbose"]; present {
			v, ok := value.(bool)
			if !ok {
				panic(fmt.Sprintf("config: Verbose must be bool, got %T", value))
			}
			cfg.Verbose = v
		}
		if value, present := layer["MaxRetries"]; present {
			v, ok := value.(int)
			if !ok {
				panic(fmt.Sprintf("config: MaxRetries must be int, got %T", value))
			}
			cfg.MaxRetries = v
		}
		if value, present := layer["LogLevel"]; present {
			v, ok := value.(string)
			if !ok {
				panic(fmt.Sprintf("config: LogLevel must be string, got %T", value))
			}
			cfg.LogLevel = v
		}
		if value, present := layer["TimeoutSec"]; present {
			v, ok := value.(int)
			if !ok {
				panic(fmt.Sprintf("config: TimeoutSec must be int, got %T", value))
			}
			cfg.TimeoutSec = v
		}
	}

	switch cfg.LogLevel {
	case "", "debug", "info", "warn", "error":
		return cfg, nil
	default:
		return Config{}, fmt.Errorf("%w: %q", ErrInvalidLogLevel, cfg.LogLevel)
	}
}
