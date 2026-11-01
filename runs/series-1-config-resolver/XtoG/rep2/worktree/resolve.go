package config

import (
	"errors"
	"fmt"
)

// ErrInvalidLogLevel is returned when the effective log level is unsupported.
var ErrInvalidLogLevel = errors.New("invalid log level")

// Config is the resolved application configuration.
type Config struct {
	Verbose    bool
	MaxRetries int
	LogLevel   string
	TimeoutSec int
}

// Resolve combines configuration layers in increasing precedence order:
// defaults, file, env, then flags. A present field overrides lower layers,
// including when its value is the Go zero value.
func Resolve(defaults, file, env, flags map[string]any) (Config, error) {
	var result Config
	layers := [...]map[string]any{defaults, file, env, flags}

	for _, layer := range layers {
		if value, ok := layer["Verbose"]; ok {
			v, valid := value.(bool)
			if !valid {
				panic(fmt.Sprintf("config: Verbose must be bool, got %T", value))
			}
			result.Verbose = v
		}
		if value, ok := layer["MaxRetries"]; ok {
			v, valid := value.(int)
			if !valid {
				panic(fmt.Sprintf("config: MaxRetries must be int, got %T", value))
			}
			result.MaxRetries = v
		}
		if value, ok := layer["LogLevel"]; ok {
			v, valid := value.(string)
			if !valid {
				panic(fmt.Sprintf("config: LogLevel must be string, got %T", value))
			}
			result.LogLevel = v
		}
		if value, ok := layer["TimeoutSec"]; ok {
			v, valid := value.(int)
			if !valid {
				panic(fmt.Sprintf("config: TimeoutSec must be int, got %T", value))
			}
			result.TimeoutSec = v
		}
	}

	switch result.LogLevel {
	case "", "debug", "info", "warn", "error":
		return result, nil
	default:
		return result, fmt.Errorf("%w: %q", ErrInvalidLogLevel, result.LogLevel)
	}
}
