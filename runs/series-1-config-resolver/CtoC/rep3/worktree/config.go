// Package config computes an effective configuration by combining ordered
// layers of in-memory values. A higher-precedence layer overrides a lower one.
package config

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// Config is the effective configuration produced by Resolve.
type Config struct {
	Verbose    bool
	MaxRetries int
	LogLevel   string
	TimeoutSec int
}

// ErrInvalidLogLevel is matched (via errors.Is) by the error Resolve returns
// when the effective LogLevel is not one of "debug", "info", "warn", "error"
// or "".
var ErrInvalidLogLevel = errors.New("config: invalid log level")

// Resolve combines four layers, in increasing precedence order
// defaults < file < env < flags, into the effective Config. Any layer may be
// nil, which is equivalent to an empty layer.
//
// A key present in a layer sets the corresponding field, even when the value
// is a zero value (false, 0, ""); among the layers presenting a field, the
// highest-precedence one wins. A key absent from a layer leaves the value from
// lower layers in place. A field presented by no layer takes its Go zero value.
//
// Legal keys are "Verbose" (bool), "MaxRetries" (int), "LogLevel" (string)
// and "TimeoutSec" (int). Resolve panics on a programmer error:
//   - a key other than the legal keys, or
//   - a present value whose dynamic Go type is not exactly the field's type.
//
// Both checks apply to every present value in every layer, including values
// that are later overridden. The panic value is an error whose message names
// the layer, the key and (for type mismatches) the dynamic type, but never the
// value itself; for a given input the message is deterministic. Unknown keys
// are echoed in the message (each truncated to maxEchoRunes runes, and at most
// maxEchoKeys of them listed), so a caller that mistakenly puts data into a
// key may see a short prefix of it there.
//
// Only the effective LogLevel is validated. If it is invalid, Resolve returns
// the effective Config together with an error matching ErrInvalidLogLevel;
// that Config is for diagnostics only and must not be used as a valid
// configuration when the error is non-nil. (This is a deliberate part of the
// contract: the spec has Resolve return the effective Config and an error.)
// The error message echoes the offending level, quoted and truncated to
// maxEchoRunes runes, because naming the bad level is what makes the error
// actionable.
//
// Resolve does not mutate or retain its inputs and is safe for concurrent use.
func Resolve(defaults, file, env, flags map[string]any) (Config, error) {
	return resolve([]layer{
		{name: "defaults", values: defaults},
		{name: "file", values: file},
		{name: "env", values: env},
		{name: "flags", values: flags},
	})
}

// layer is one named source of presented values. A nil values map means every
// field is absent.
type layer struct {
	name   string // used only in panic messages
	values map[string]any
}

// resolve applies layers from lowest to highest precedence, then validates
// the merged result.
func resolve(layers []layer) (Config, error) {
	var c Config
	for _, l := range layers {
		l.apply(&c)
	}
	return c, validate(&c, bindings)
}

// apply stores every value presented by l into c. Keys are visited in the
// fixed bindings order (never map order), so when a layer contains several
// bad entries the panic is reproducible: unknown keys are reported first
// (all of them, sorted), then the first wrong-typed value in field order.
func (l layer) apply(c *Config) {
	if unknown := l.unknownKeys(); len(unknown) > 0 {
		panic(fmt.Errorf("config: layer %q: unknown key(s) %s",
			l.name, strings.Join(unknown, ", ")))
	}
	for i := range bindings {
		b := &bindings[i]
		if v, present := l.values[b.key]; present {
			b.set(c, l.name, v)
		}
	}
}

// unknownKeys returns the keys of l that have no binding, sorted, quoted and
// truncated for display. At most maxEchoKeys keys are listed; any remainder is
// summarized as a count.
func (l layer) unknownKeys() []string {
	var unknown []string
	for key := range l.values {
		if _, ok := knownKeys[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	slices.Sort(unknown)
	out := make([]string, 0, min(len(unknown), maxEchoKeys)+1)
	for _, key := range unknown[:min(len(unknown), maxEchoKeys)] {
		out = append(out, quoteTrunc(key))
	}
	if extra := len(unknown) - maxEchoKeys; extra > 0 {
		out = append(out, fmt.Sprintf("and %d more", extra))
	}
	return out
}

// Limits on how much caller-supplied text is echoed into panic and error
// messages, so that misplaced data cannot flood or leak wholesale into logs.
const (
	maxEchoRunes = 32
	maxEchoKeys  = 5
)

// quoteTrunc quotes s with %q, truncating it to maxEchoRunes runes first; a
// truncated string is marked with a trailing "...".
func quoteTrunc(s string) string {
	if utf8.RuneCountInString(s) <= maxEchoRunes {
		return fmt.Sprintf("%q", s)
	}
	runes := []rune(s)
	return fmt.Sprintf("%q...", string(runes[:maxEchoRunes]))
}

// validate runs every validator in bs, in order, against the merged Config.
// A single failure is returned as-is (so errors.Unwrap works on it); several
// failures are combined with errors.Join.
//
// Validators are per-field. A future cross-field rule (one that reads several
// fields) belongs here, as an extra pass after the per-field loop.
func validate(c *Config, bs []binding) error {
	var errs []error
	for _, b := range bs {
		if b.validate == nil {
			continue
		}
		if err := b.validate(c); err != nil {
			errs = append(errs, err)
		}
	}
	switch len(errs) {
	case 0:
		return nil
	case 1:
		return errs[0]
	default:
		return errors.Join(errs...)
	}
}

// binding ties one legal key to one Config field: how to type-check and store
// a presented value, and optionally how to validate the effective value.
type binding struct {
	key      string
	set      func(c *Config, layerName string, v any) // type-checks v (panics) and stores it
	validate func(c *Config) error                    // nil means "not validated"
}

// bindings lists every Config field in declaration order, so validation runs
// in a deterministic order. Adding a field means adding the struct field and
// one entry here.
var bindings = []binding{
	bind("Verbose", func(c *Config) *bool { return &c.Verbose }, nil),
	bind("MaxRetries", func(c *Config) *int { return &c.MaxRetries }, nil),
	bind("LogLevel", func(c *Config) *string { return &c.LogLevel }, validateLogLevel),
	bind("TimeoutSec", func(c *Config) *int { return &c.TimeoutSec }, nil),
}

// knownKeys is the set of legal keys. It is read-only after initialization.
var knownKeys = indexBindings(bindings)

// indexBindings returns the set of keys in bs, panicking on a duplicate key.
func indexBindings(bs []binding) map[string]struct{} {
	m := make(map[string]struct{}, len(bs))
	for _, b := range bs {
		if _, dup := m[b.key]; dup {
			panic(fmt.Errorf("config: duplicate binding for key %q", b.key))
		}
		m[b.key] = struct{}{}
	}
	return m
}

// bind builds the binding for a field of type T. The type assertion is exact:
// a value of any other dynamic type (including named types with the same
// underlying type, and nil) panics. Panic messages never include the value,
// so secret-bearing fields cannot leak into crash logs.
func bind[T any](key string, field func(*Config) *T, check func(T) error) binding {
	b := binding{key: key}
	b.set = func(c *Config, layerName string, v any) {
		t, ok := v.(T)
		if !ok {
			panic(fmt.Errorf("config: layer %q, key %q: value has type %T, want %T",
				layerName, key, v, *new(T)))
		}
		*field(c) = t
	}
	if check != nil {
		b.validate = func(c *Config) error { return check(*field(c)) }
	}
	return b
}

// validateLogLevel accepts exactly the legal levels (case-sensitive, no
// trimming); "" is an explicit, legal empty level.
func validateLogLevel(s string) error {
	switch s {
	case "", "debug", "info", "warn", "error":
		return nil
	}
	return fmt.Errorf("%w: %s", ErrInvalidLogLevel, quoteTrunc(s))
}
