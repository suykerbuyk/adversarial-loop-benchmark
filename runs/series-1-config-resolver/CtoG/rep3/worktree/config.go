// Package config computes an effective configuration from ordered layers.
//
// Layers are plain in-memory maps supplied by the caller; the package never
// reads the environment, files, or the command line.
package config

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
)

// Config is the effective configuration produced by [Resolve].
type Config struct {
	Verbose    bool
	MaxRetries int
	LogLevel   string
	TimeoutSec int
}

// ErrInvalidLogLevel is returned (possibly wrapped) by [Resolve] when the
// effective LogLevel is not one of "debug", "info", "warn", "error", or "".
// Match it with errors.Is.
var ErrInvalidLogLevel = errors.New("config: invalid log level")

// Resolve merges four layers, given from lowest to highest precedence
// (defaults, file, env, flags), into an effective Config.
//
// A field is presented by a layer when its key exists in the layer's map;
// a presented value overrides lower layers even when it is a zero value
// (false, 0, ""). An absent key leaves the lower layers' value in place.
// A field that no layer presents keeps its Go zero value. A nil map is a
// layer in which every field is absent.
//
// The legal keys are "Verbose" (bool), "MaxRetries" (int), "LogLevel"
// (string), and "TimeoutSec" (int). An unknown key, or a present value whose
// dynamic type is not exactly the field's type (including an explicit nil
// value), is a programmer error and causes a panic. This is checked in every
// layer, including values that a higher layer overrides.
//
// Validation applies only to the effective values. If it fails, Resolve
// returns the zero Config and an error; for an invalid LogLevel the error
// satisfies errors.Is(err, ErrInvalidLogLevel). Callers must not rely on any
// partially merged value when err != nil.
//
// Resolve does not mutate or retain its arguments and is safe for
// concurrent use.
func Resolve(defaults, file, env, flags map[string]any) (Config, error) {
	return resolve(
		layer{name: "defaults", values: defaults},
		layer{name: "file", values: file},
		layer{name: "env", values: env},
		layer{name: "flags", values: flags},
	)
}

// layer is one named source of field values. A nil values map is legal and
// means every field is absent.
type layer struct {
	name   string // used only in panic messages
	values map[string]any
}

// field describes one Config field: its map key, its expected type name (for
// diagnostics), and how to store a value into a Config.
type field struct {
	key    string
	typ    string
	assign func(c *Config, v any) bool // false if v has the wrong type
}

// bind builds a field whose value must have dynamic type exactly T.
func bind[T any](key string, ptr func(*Config) *T) field {
	return field{
		key: key,
		typ: reflect.TypeFor[T]().String(),
		assign: func(c *Config, v any) bool {
			t, ok := v.(T)
			if !ok {
				return false
			}
			*ptr(c) = t
			return true
		},
	}
}

// fields is the single source of truth for the legal keys. Adding a Config
// field requires exactly one entry here.
var fields = []field{
	bind("Verbose", func(c *Config) *bool { return &c.Verbose }),
	bind("MaxRetries", func(c *Config) *int { return &c.MaxRetries }),
	bind("LogLevel", func(c *Config) *string { return &c.LogLevel }),
	bind("TimeoutSec", func(c *Config) *int { return &c.TimeoutSec }),
}

// fieldIndex maps each legal key to its position in fields.
var fieldIndex = indexFields(fields)

func indexFields(fs []field) map[string]int {
	idx := make(map[string]int, len(fs))
	for i, f := range fs {
		if _, dup := idx[f.key]; dup {
			panic("config: duplicate field key " + strconv.Quote(f.key))
		}
		idx[f.key] = i
	}
	return idx
}

// validator checks one aspect of the effective Config. Each validator wraps
// its own sentinel with %w so callers can match it with errors.Is.
type validator func(Config) error

var validators = []validator{
	validateLogLevel,
}

func validateLogLevel(c Config) error {
	switch c.LogLevel {
	case "debug", "info", "warn", "error", "":
		return nil
	}
	return fmt.Errorf("%w: %q", ErrInvalidLogLevel, c.LogLevel)
}

// validate runs every validator and joins their failures; nil if all pass.
func validate(c Config) error {
	var errs []error
	for _, v := range validators {
		if err := v(c); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// resolve applies layers from lowest to highest precedence, then validates
// the effective result.
func resolve(layers ...layer) (Config, error) {
	var cfg Config
	for _, l := range layers {
		checkKeys(l)
		for _, f := range fields {
			v, present := l.values[f.key]
			if !present {
				continue
			}
			if !f.assign(&cfg, v) {
				panic(fmt.Sprintf("config: layer %q: key %q must be %s, got %T", l.name, f.key, f.typ, v))
			}
		}
	}
	if err := validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// checkKeys panics if l contains any key that is not a known field.
func checkKeys(l layer) {
	var unknown []string
	for k := range l.values {
		if _, ok := fieldIndex[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return
	}
	slices.Sort(unknown)
	panic(fmt.Sprintf("config: layer %q: unknown key(s) %q", l.name, unknown))
}
