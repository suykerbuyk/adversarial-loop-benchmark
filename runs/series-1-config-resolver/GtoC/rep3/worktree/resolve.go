package config

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Config is the effective configuration after layer resolution.
type Config struct {
	Verbose    bool
	MaxRetries int
	LogLevel   string
	TimeoutSec int
}

// ErrInvalidLogLevel is the sentinel for an effective LogLevel outside the
// legal set. Resolve wraps it; callers match with errors.Is.
var ErrInvalidLogLevel = errors.New("config: invalid log level")

// layer is one precedence step. name is only for panic messages.
type layer struct {
	name   string
	values map[string]any
}

// field binds one legal key to one Config field.
// apply type-checks v with an exact type assertion and stores it.
// A failed assertion panics; there is no conversion.
type field struct {
	key   string
	apply func(c *Config, layerName string, v any)
}

// unknownKeyLimit caps how much of a bad layer is echoed in a panic so a
// huge or hostile key set cannot blow up a log line. The cap is a debugging
// aid, not part of the Resolve contract.
const (
	unknownKeyLimit     = 5
	unknownKeyRuneLimit = 32
)

// bind attaches key to a typed setter. The key is named once. T is the only
// dynamic type accepted; named types, wider integers, and nil fail the
// assertion and panic.
func bind[T any](key string, set func(*Config, T)) field {
	var zero T
	want := fmt.Sprintf("%T", zero)
	return field{
		key: key,
		apply: func(c *Config, layerName string, v any) {
			n, ok := v.(T)
			if !ok {
				panicWrongType(layerName, key, v, want)
			}
			set(c, n)
		},
	}
}

var fields = []field{
	bind("Verbose", func(c *Config, v bool) { c.Verbose = v }),
	bind("MaxRetries", func(c *Config, v int) { c.MaxRetries = v }),
	bind("LogLevel", func(c *Config, v string) { c.LogLevel = v }),
	bind("TimeoutSec", func(c *Config, v int) { c.TimeoutSec = v }),
}

var knownKeys = indexFields(fields)

var legalLogLevels = map[string]struct{}{
	"":      {},
	"debug": {},
	"info":  {},
	"warn":  {},
	"error": {},
}

// indexFields builds the legal-key set. A duplicated key is a programmer error.
func indexFields(fs []field) map[string]struct{} {
	known := make(map[string]struct{}, len(fs))
	for _, f := range fs {
		if _, dup := known[f.key]; dup {
			panic(fmt.Errorf("config: duplicate field key %q", f.key))
		}
		known[f.key] = struct{}{}
	}
	return known
}

func panicWrongType(layerName, key string, v any, want string) {
	// %T prints the dynamic type only. The value itself is omitted: a
	// wrong-typed present value may be an arbitrary object.
	panic(fmt.Errorf("config: layer %q: key %q: value has type %T, want %s", layerName, key, v, want))
}

// Resolve combines defaults, file, env, and flags (lowest to highest
// precedence) into one Config.
//
// A key present in a layer sets that field, including when the value is
// false, 0, or "". A key absent from a layer leaves the field unchanged.
// A field presented by no layer keeps its Go zero value.
//
// Only the keys "Verbose", "MaxRetries", "LogLevel", and "TimeoutSec" are
// legal. Any other key panics. Unknown keys are not ignored. A present
// value whose dynamic type is not the field's type (bool, int, string, int
// respectively) panics, including when a higher layer later overrides that
// field. Integer fields accept only dynamic type int (not int32, int64,
// uint, float64, or a named integer type). LogLevel accepts only string,
// not a named string type. Verbose accepts only bool. An untyped nil stored
// under a legal key is present and panics as a wrong type.
//
// Panic values are errors. The text names the layer and the key. A wrong
// type also names the expected type and the value's dynamic type, and does
// not print the value.
//
// Validation runs only on the effective LogLevel. Legal values are
// "debug", "info", "warn", "error", and "". Any other effective value
// returns the resolved Config (invalid LogLevel included) and an error for
// which errors.Is(err, ErrInvalidLogLevel) is true. The error quotes the
// effective level so the caller can see which string was rejected.
// Overridden presented levels are not validated. Verbose, MaxRetries, and
// TimeoutSec are not validated.
//
// A nil map is an empty layer: every field is absent. Resolve does not read
// the environment, files, or the command line, and does not retain the maps
// after return. The caller must not mutate the maps concurrently with Resolve.
func Resolve(defaults, file, env, flags map[string]any) (Config, error) {
	return resolveLayers([]layer{
		{name: "defaults", values: defaults},
		{name: "file", values: file},
		{name: "env", values: env},
		{name: "flags", values: flags},
	})
}

// resolveLayers applies layers from lowest precedence to highest. A new
// layer is one entry in this slice; Resolve is the fixed four-layer entry.
func resolveLayers(layers []layer) (Config, error) {
	var c Config
	for _, l := range layers {
		l.apply(&c)
	}
	return c, validate(c)
}

func (l layer) apply(c *Config) {
	if l.values == nil {
		return
	}
	var unknown []string
	for k := range l.values {
		if _, ok := knownKeys[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		panic(fmt.Errorf("config: layer %q: unknown key(s) %s", l.name, formatUnknownKeys(unknown)))
	}
	for _, f := range fields {
		if v, ok := l.values[f.key]; ok {
			f.apply(c, l.name, v)
		}
	}
}

// formatUnknownKeys sorts keys and echoes a bounded, deterministic list.
// At most unknownKeyLimit keys are shown. Each shown key is limited to
// unknownKeyRuneLimit runes, with a trailing "..." if truncated. Further
// keys are summarized as "and N more".
func formatUnknownKeys(keys []string) string {
	slices.Sort(keys)
	shown := keys
	extra := 0
	if len(keys) > unknownKeyLimit {
		shown = keys[:unknownKeyLimit]
		extra = len(keys) - unknownKeyLimit
	}
	parts := make([]string, len(shown))
	for i, k := range shown {
		parts[i] = strconv.Quote(truncateKey(k))
	}
	msg := strings.Join(parts, ", ")
	if extra > 0 {
		msg += fmt.Sprintf(" and %d more", extra)
	}
	return msg
}

func truncateKey(key string) string {
	runes := []rune(key)
	if len(runes) <= unknownKeyRuneLimit {
		return key
	}
	return string(runes[:unknownKeyRuneLimit]) + "..."
}

func validate(c Config) error {
	if _, ok := legalLogLevels[c.LogLevel]; !ok {
		return fmt.Errorf("%w: %q", ErrInvalidLogLevel, c.LogLevel)
	}
	return nil
}
