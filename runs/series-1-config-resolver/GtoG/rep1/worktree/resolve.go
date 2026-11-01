package config

import (
	"errors"
	"fmt"
)

// ErrInvalidLogLevel is returned when the effective LogLevel is not one of
// "", "debug", "info", "warn", or "error". Lower-layer values that a higher
// layer overrides are not validated.
var ErrInvalidLogLevel = errors.New("config: invalid log level")

// Config is the effective configuration after layer resolution.
// A field no layer presents keeps its Go zero value.
type Config struct {
	Verbose    bool
	MaxRetries int
	LogLevel   string
	TimeoutSec int
}

// fieldKind identifies one legal key and the Go type it must have.
type fieldKind int

const (
	fieldVerbose fieldKind = iota
	fieldMaxRetries
	fieldLogLevel
	fieldTimeoutSec
	fieldCount
)

// layer is one precedence slot. name is included in panic messages so a
// caller can see which map was wrong. Adding a layer is appending to the
// slice in Resolve.
type layer struct {
	name   string
	values map[string]any
}

// setState records whether a field has been presented and, if so, its value.
// Presence is separate from the value: false, 0, and "" are presented.
// Only the slot matching the field is meaningful.
type setState struct {
	presented bool
	b         bool
	i         int
	s         string
}

// Resolve combines four layers in ascending precedence: defaults, file, env,
// flags. A nil map is an empty layer. A key present in a map sets that field,
// including when the value is a zero value. A key absent from a map leaves
// lower layers unchanged.
//
// A key other than "Verbose", "MaxRetries", "LogLevel", or "TimeoutSec", or a
// present value of the wrong dynamic type, is a programmer error and panics.
// Accepted types are exact: bool, int, and string respectively. int32, int64,
// float64, and a nil interface stored under a key panic.
//
// Verbose, MaxRetries, and TimeoutSec are not validated. Validation applies
// only to the effective LogLevel.
func Resolve(defaults, file, env, flags map[string]any) (Config, error) {
	layers := []layer{
		{name: "defaults", values: defaults},
		{name: "file", values: file},
		{name: "env", values: env},
		{name: "flags", values: flags},
	}

	var state [fieldCount]setState
	for _, ly := range layers {
		for key, val := range ly.values {
			apply(ly.name, key, val, &state)
		}
	}

	cfg := materialize(state)
	if !legalLogLevel(cfg.LogLevel) {
		return Config{}, ErrInvalidLogLevel
	}
	return cfg, nil
}

func apply(layerName, key string, val any, state *[fieldCount]setState) {
	kind, ok := kindOf(key)
	if !ok {
		panic(fmt.Sprintf("config: layer %s: unknown key %q", layerName, key))
	}
	switch kind {
	case fieldVerbose:
		b, ok := val.(bool)
		if !ok {
			panic(fmt.Sprintf("config: layer %s: key %q: want bool, got %T", layerName, key, val))
		}
		state[kind] = setState{presented: true, b: b}
	case fieldMaxRetries, fieldTimeoutSec:
		n, ok := val.(int)
		if !ok {
			panic(fmt.Sprintf("config: layer %s: key %q: want int, got %T", layerName, key, val))
		}
		state[kind] = setState{presented: true, i: n}
	case fieldLogLevel:
		s, ok := val.(string)
		if !ok {
			panic(fmt.Sprintf("config: layer %s: key %q: want string, got %T", layerName, key, val))
		}
		state[kind] = setState{presented: true, s: s}
	default:
		// A new fieldKind was added without an apply branch.
		panic(fmt.Sprintf("config: layer %s: unhandled field %q", layerName, key))
	}
}

func kindOf(key string) (fieldKind, bool) {
	switch key {
	case "Verbose":
		return fieldVerbose, true
	case "MaxRetries":
		return fieldMaxRetries, true
	case "LogLevel":
		return fieldLogLevel, true
	case "TimeoutSec":
		return fieldTimeoutSec, true
	default:
		return 0, false
	}
}

func materialize(state [fieldCount]setState) Config {
	var cfg Config
	if state[fieldVerbose].presented {
		cfg.Verbose = state[fieldVerbose].b
	}
	if state[fieldMaxRetries].presented {
		cfg.MaxRetries = state[fieldMaxRetries].i
	}
	if state[fieldLogLevel].presented {
		cfg.LogLevel = state[fieldLogLevel].s
	}
	if state[fieldTimeoutSec].presented {
		cfg.TimeoutSec = state[fieldTimeoutSec].i
	}
	return cfg
}

func legalLogLevel(level string) bool {
	switch level {
	case "", "debug", "info", "warn", "error":
		return true
	default:
		return false
	}
}
