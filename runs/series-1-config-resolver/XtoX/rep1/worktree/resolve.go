package config

import (
	"errors"
	"fmt"
)

// ErrInvalidLogLevel indicates that the effective log level is unsupported.
var ErrInvalidLogLevel = errors.New("invalid log level")

// Config is the effective configuration produced by Resolve.
type Config struct {
	Verbose    bool
	MaxRetries int
	LogLevel   string
	TimeoutSec int
}

// Resolve combines configuration layers from lowest to highest precedence.
// A present field replaces the value from lower-precedence layers, including
// when its value is the Go zero value. A present value with the wrong type
// panics because it indicates a caller programming error.
func Resolve(defaults, file, env, flags map[string]any) (Config, error) {
	var config Config
	for _, layer := range [...]map[string]any{defaults, file, env, flags} {
		applyLayer(&config, layer)
	}

	switch config.LogLevel {
	case "", "debug", "info", "warn", "error":
		return config, nil
	default:
		return config, fmt.Errorf("%w: %q", ErrInvalidLogLevel, config.LogLevel)
	}
}

func applyLayer(config *Config, layer map[string]any) {
	for key, value := range layer {
		switch key {
		case "Verbose":
			v, ok := value.(bool)
			if !ok {
				panicType(key, "bool", value)
			}
			config.Verbose = v
		case "MaxRetries":
			v, ok := value.(int)
			if !ok {
				panicType(key, "int", value)
			}
			config.MaxRetries = v
		case "LogLevel":
			v, ok := value.(string)
			if !ok {
				panicType(key, "string", value)
			}
			config.LogLevel = v
		case "TimeoutSec":
			v, ok := value.(int)
			if !ok {
				panicType(key, "int", value)
			}
			config.TimeoutSec = v
		}
	}
}

func panicType(key, want string, got any) {
	panic(fmt.Sprintf("config: %s must have type %s, got %T", key, want, got))
}
