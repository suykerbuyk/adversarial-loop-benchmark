package config

import (
	"errors"
	"fmt"
)

// ErrInvalidLogLevel is returned when the effective LogLevel is not one of
// "", "debug", "info", "warn", "error".
//
// Resolve wraps it with %w so errors.Is(err, ErrInvalidLogLevel) is true.
// The wrapped error names the rejected value. Callers must not depend on the
// exact error string.
var ErrInvalidLogLevel error = errors.New("invalid log level")

// Config is the effective configuration after layer merge and validation.
// Zero values are legal results (false, 0, "", 0).
type Config struct {
	Verbose    bool
	MaxRetries int
	LogLevel   string
	TimeoutSec int
}

// fieldID identifies a Config field in the descriptor table.
type fieldID int

const (
	fieldVerbose fieldID = iota
	fieldMaxRetries
	fieldLogLevel
	fieldTimeoutSec
)

// fieldSpec describes one legal layer key. apply panics if v has the wrong
// dynamic type, otherwise it writes the field.
type fieldSpec struct {
	key   string
	id    fieldID
	apply func(cfg *Config, v any)
}

var fields = []fieldSpec{
	{key: "Verbose", id: fieldVerbose, apply: applyVerbose},
	{key: "MaxRetries", id: fieldMaxRetries, apply: applyMaxRetries},
	{key: "LogLevel", id: fieldLogLevel, apply: applyLogLevel},
	{key: "TimeoutSec", id: fieldTimeoutSec, apply: applyTimeoutSec},
}

// fieldByKey maps a legal layer key to its descriptor. Built once in init.
var fieldByKey map[string]*fieldSpec

func init() {
	fieldByKey = make(map[string]*fieldSpec, len(fields))
	for i := range fields {
		fieldByKey[fields[i].key] = &fields[i]
	}
}

// legalLogLevels is the set of effective LogLevel values Resolve accepts.
// Membership is exact and case-sensitive. The empty string is legal.
var legalLogLevels = map[string]struct{}{
	"":      {},
	"debug": {},
	"info":  {},
	"warn":  {},
	"error": {},
}

func applyVerbose(cfg *Config, v any) {
	b, ok := v.(bool)
	if !ok {
		panic(fmt.Sprintf("config: key %q has type %T, want bool", "Verbose", v))
	}
	cfg.Verbose = b
}

func applyMaxRetries(cfg *Config, v any) {
	n, ok := v.(int)
	if !ok {
		panic(fmt.Sprintf("config: key %q has type %T, want int", "MaxRetries", v))
	}
	cfg.MaxRetries = n
}

func applyLogLevel(cfg *Config, v any) {
	s, ok := v.(string)
	if !ok {
		panic(fmt.Sprintf("config: key %q has type %T, want string", "LogLevel", v))
	}
	cfg.LogLevel = s
}

func applyTimeoutSec(cfg *Config, v any) {
	n, ok := v.(int)
	if !ok {
		panic(fmt.Sprintf("config: key %q has type %T, want int", "TimeoutSec", v))
	}
	cfg.TimeoutSec = n
}

// Resolve merges four layers in ascending precedence and validates the
// effective LogLevel.
//
// Precedence, lowest to highest: defaults, file, env, flags.
// A key present in a map sets that field, including zero values
// (false, 0, ""). A key absent from a map, or a nil map, leaves the field
// unchanged. If no layer presents a field, the result is the Go zero value.
//
// Legal keys are "Verbose", "MaxRetries", "LogLevel", "TimeoutSec".
// A present value whose dynamic type is wrong for its key panics.
// An unknown key panics (programmer error).
//
// Validation runs only on the effective LogLevel, after every layer has been
// applied. A presented LogLevel that a higher layer overrides is not validated.
// Verbose, MaxRetries, and TimeoutSec are not validated.
//
// On validation failure the returned Config is the zero Config. Callers must
// not use the config when err != nil.
func Resolve(defaults, file, env, flags map[string]any) (Config, error) {
	var cfg Config
	for _, layer := range []map[string]any{defaults, file, env, flags} {
		if layer == nil {
			continue
		}
		for key, val := range layer {
			spec, ok := fieldByKey[key]
			if !ok {
				panic(fmt.Sprintf("config: unknown key %q", key))
			}
			spec.apply(&cfg, val)
		}
	}
	if _, ok := legalLogLevels[cfg.LogLevel]; !ok {
		return Config{}, fmt.Errorf("config: log level %q: %w", cfg.LogLevel, ErrInvalidLogLevel)
	}
	return cfg, nil
}
