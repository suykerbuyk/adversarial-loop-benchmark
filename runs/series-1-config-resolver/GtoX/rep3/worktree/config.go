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

// fieldKind is the required dynamic type of a presented field value.
type fieldKind int

const (
	kindBool fieldKind = iota
	kindInt
	kindString
)

// fieldSpec describes one legal configuration key.
// Adding a field is a Config field plus one row in fields.
type fieldSpec struct {
	name string
	kind fieldKind
	// apply writes a type-checked value into cfg. Called only when the key is present.
	apply func(cfg *Config, v any)
}

// fields is the registry of legal keys. Layer walks consult this table;
// they do not switch on field names themselves.
var fields = []fieldSpec{
	{name: "Verbose", kind: kindBool, apply: func(cfg *Config, v any) { cfg.Verbose = v.(bool) }},
	{name: "MaxRetries", kind: kindInt, apply: func(cfg *Config, v any) { cfg.MaxRetries = v.(int) }},
	{name: "LogLevel", kind: kindString, apply: func(cfg *Config, v any) { cfg.LogLevel = v.(string) }},
	{name: "TimeoutSec", kind: kindInt, apply: func(cfg *Config, v any) { cfg.TimeoutSec = v.(int) }},
}

// fieldByName indexes fields for presence checks. Built once from fields.
var fieldByName map[string]fieldSpec

func init() {
	fieldByName = make(map[string]fieldSpec, len(fields))
	for _, f := range fields {
		fieldByName[f.name] = f
	}
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
// The slice of maps inside Resolve is the extension point for more layers
// if the public signature is later allowed to change.
//
// A nil map is an empty layer: every field is absent. A key present in a
// map sets that field, including zero values (false, 0, ""). A key absent
// from a map leaves the field unchanged. If no layer presents a field, the
// effective value is that field's Go zero value.
//
// A present value of the wrong Go type for its key panics. An unknown key
// panics. Both are programmer errors. Resolve does not read the environment,
// any file, or the command line.
//
// Validation runs only on the effective LogLevel, after every layer has
// been applied. A presented LogLevel overridden by a higher layer is never
// validated. Verbose, MaxRetries, and TimeoutSec are not validated.
func Resolve(defaults, file, env, flags map[string]any) (Config, error) {
	var cfg Config
	layers := []map[string]any{defaults, file, env, flags}
	for _, layer := range layers {
		applyLayer(&cfg, layer)
	}
	if _, ok := legalLogLevels[cfg.LogLevel]; !ok {
		return Config{}, fmt.Errorf("%w: %q", ErrInvalidLogLevel, cfg.LogLevel)
	}
	return cfg, nil
}

// applyLayer overlays one layer onto cfg. A nil or empty layer is a no-op.
func applyLayer(cfg *Config, layer map[string]any) {
	if layer == nil {
		return
	}
	for key, value := range layer {
		spec, ok := fieldByName[key]
		if !ok {
			panic(fmt.Sprintf("config: unknown key %q", key))
		}
		mustMatchKind(spec, value)
		spec.apply(cfg, value)
	}
}

// mustMatchKind panics if v is not the exact dynamic type required by spec.
// Typed aliases, other numeric kinds, and nil are programmer errors.
func mustMatchKind(spec fieldSpec, v any) {
	var want string
	var ok bool
	switch spec.kind {
	case kindBool:
		want = "bool"
		_, ok = v.(bool)
	case kindInt:
		want = "int"
		_, ok = v.(int)
	case kindString:
		want = "string"
		_, ok = v.(string)
	default:
		panic(fmt.Sprintf("config: key %q: unknown field kind %d", spec.name, spec.kind))
	}
	if !ok {
		panic(fmt.Sprintf("config: key %q: expected %s, got %T", spec.name, want, v))
	}
}
