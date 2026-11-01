package config

import (
	"errors"
	"fmt"
)

// ErrInvalidLogLevel is returned when the effective LogLevel is not one of
// the legal values: "debug", "info", "warn", "error", or "".
var ErrInvalidLogLevel = errors.New("config: invalid log level")

// Config is the effective configuration after layer resolution.
type Config struct {
	Verbose    bool
	MaxRetries int
	LogLevel   string
	TimeoutSec int
}

// fieldSpec describes one resolvable field. apply writes a type-checked
// presented value into cfg. A wrong dynamic type panics. The closure is the
// only type mechanism: it both asserts the dynamic type and assigns the field.
type fieldSpec struct {
	apply func(cfg *Config, v any)
}

// fields is the extension point for new configuration fields. Adding a field
// is a Config member plus one row here (and a post-resolve check if the
// effective value needs validation).
var fields = []struct {
	name string
	spec fieldSpec
}{
	{
		name: "Verbose",
		spec: fieldSpec{apply: func(cfg *Config, v any) {
			cfg.Verbose = mustBool("Verbose", v)
		}},
	},
	{
		name: "MaxRetries",
		spec: fieldSpec{apply: func(cfg *Config, v any) {
			cfg.MaxRetries = mustInt("MaxRetries", v)
		}},
	},
	{
		name: "LogLevel",
		spec: fieldSpec{apply: func(cfg *Config, v any) {
			cfg.LogLevel = mustString("LogLevel", v)
		}},
	},
	{
		name: "TimeoutSec",
		spec: fieldSpec{apply: func(cfg *Config, v any) {
			cfg.TimeoutSec = mustInt("TimeoutSec", v)
		}},
	},
}

var fieldByName = map[string]fieldSpec{}

func init() {
	for _, f := range fields {
		fieldByName[f.name] = f.spec
	}
}

func mustBool(key string, v any) bool {
	b, ok := v.(bool)
	if !ok {
		panic(fmt.Sprintf("config: key %q: expected bool, got %T", key, v))
	}
	return b
}

func mustInt(key string, v any) int {
	n, ok := v.(int)
	if !ok {
		panic(fmt.Sprintf("config: key %q: expected int, got %T", key, v))
	}
	return n
}

func mustString(key string, v any) string {
	s, ok := v.(string)
	if !ok {
		panic(fmt.Sprintf("config: key %q: expected string, got %T", key, v))
	}
	return s
}

// legalLogLevels is the set of accepted effective LogLevel values.
// Comparison is exact and case-sensitive. "" is legal.
var legalLogLevels = map[string]struct{}{
	"":      {},
	"debug": {},
	"info":  {},
	"warn":  {},
	"error": {},
}

// Resolve combines four ordered layers into an effective Config.
// Precedence is argument order, low to high: defaults, file, env, flags.
// The internal slice of maps is the extension point for additional layers;
// this signature stays fixed at four arguments.
// A nil map is an empty layer (every field absent).
// A key present in a map sets that field, including zero values.
// A key absent from a map leaves the field unchanged.
// A present value of the wrong Go type for its key panics.
// Unknown keys panic.
// Validation runs only on the effective LogLevel.
func Resolve(defaults, file, env, flags map[string]any) (Config, error) {
	var cfg Config
	layers := []map[string]any{defaults, file, env, flags}
	for _, layer := range layers {
		if layer == nil {
			continue
		}
		for key, value := range layer {
			spec, ok := fieldByName[key]
			if !ok {
				panic(fmt.Sprintf("config: unknown key %q", key))
			}
			spec.apply(&cfg, value)
		}
	}
	if _, ok := legalLogLevels[cfg.LogLevel]; !ok {
		return Config{}, fmt.Errorf("%w: %q", ErrInvalidLogLevel, cfg.LogLevel)
	}
	return cfg, nil
}
