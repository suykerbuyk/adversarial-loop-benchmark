package config

import (
	"errors"
	"fmt"
	"sort"
)

// ErrInvalidLogLevel is returned when the effective LogLevel is not a legal value.
var ErrInvalidLogLevel = errors.New("invalid log level")

// Config is the effective configuration after layer resolution.
type Config struct {
	Verbose    bool
	MaxRetries int
	LogLevel   string
	TimeoutSec int
}

// field is one registered configuration slot.
// apply writes a presented value into dst. validate, if set, checks the
// effective value after every layer has been applied. LogLevel's allow-list
// is not a row validator; see checkLogLevel.
type field struct {
	key      string
	apply    func(dst *Config, v any)
	validate func(cfg Config) error
}

// fieldOf binds key to exactly one Config field. The pointer function is the
// binding: apply writes through whatever dst returns. A wrong dynamic type,
// including a named type whose underlying type matches T and including an
// untyped nil, panics. The panic names the key and the types only.
func fieldOf[T any](key string, dst func(*Config) *T) field {
	return field{
		key: key,
		apply: func(cfg *Config, v any) {
			typed, ok := v.(T)
			if !ok {
				panic(fmt.Sprintf("config: key %q: got %T, want %s", key, v, typeName[T]()))
			}
			*dst(cfg) = typed
		},
	}
}

func typeName[T any]() string {
	var zero T
	return fmt.Sprintf("%T", zero)
}

// fields is the registry. Order is the order of wrong-type panics within a
// layer. A new field is a Config member plus one row here.
var fields = []field{
	fieldOf("Verbose", func(c *Config) *bool { return &c.Verbose }),
	fieldOf("MaxRetries", func(c *Config) *int { return &c.MaxRetries }),
	fieldOf("LogLevel", func(c *Config) *string { return &c.LogLevel }),
	fieldOf("TimeoutSec", func(c *Config) *int { return &c.TimeoutSec }),
}

// Resolve combines defaults, file, env, and flags in increasing precedence.
//
// A key present in a map sets that field, including zero values (false, 0, "").
// A key absent from a map leaves the field unchanged. A nil map is all-absent.
// If no layer presents a field, the effective value is that field's Go zero value.
//
// Legal keys are "Verbose", "MaxRetries", "LogLevel", and "TimeoutSec".
// A present value of the wrong Go type panics, including when a higher layer
// later overrides that field, including an untyped nil, and including a named
// type whose underlying type matches (type myInt int is not int). The panic
// names the key, the expected type, and the dynamic type (%T only).
// Unknown keys are ignored in v1 (see noteUnknownKey). That choice lives in
// noteUnknownKey and can be flipped without touching the merge walk.
//
// Only the effective LogLevel is validated, against "", "debug", "info",
// "warn", and "error", case-sensitively. A presented level that a higher
// layer overrides is never validated. Verbose, MaxRetries, and TimeoutSec
// are not validated. The LogLevel check is independent of any other field
// validator: an illegal effective LogLevel always yields an error for which
// errors.Is(err, ErrInvalidLogLevel) is true, even if another field is also
// invalid.
//
// When err != nil, the returned Config is unspecified. Callers must not
// read it. This version returns the merged struct (not a zeroed Config);
// that is an implementation detail, not a contract. The error wraps
// ErrInvalidLogLevel.
//
// Resolve only reads the maps. Callers must not mutate them during the call.
func Resolve(defaults, file, env, flags map[string]any) (Config, error) {
	return resolve([]map[string]any{defaults, file, env, flags})
}

// resolve walks layers from lowest precedence to highest. The last element wins
// for each presented key. A later ResolveLayers can call this without forking
// the walk. The exported signature stays four positional maps.
func resolve(layers []map[string]any) (Config, error) {
	var cfg Config
	known := knownKeys()
	for _, layer := range layers {
		if layer == nil {
			continue
		}
		for i := range fields {
			if v, ok := layer[fields[i].key]; ok {
				fields[i].apply(&cfg, v)
			}
		}
		keys := make([]string, 0, len(layer))
		for key := range layer {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			noteUnknownKey(key, known)
		}
	}

	var rowErr error
	for i := range fields {
		if fields[i].validate == nil {
			continue
		}
		if err := fields[i].validate(cfg); err != nil && rowErr == nil {
			rowErr = err
		}
	}
	if err := checkLogLevel(cfg.LogLevel); err != nil {
		return cfg, err
	}
	if rowErr != nil {
		return cfg, rowErr
	}
	return cfg, nil
}

func knownKeys() map[string]struct{} {
	known := make(map[string]struct{}, len(fields))
	for i := range fields {
		known[fields[i].key] = struct{}{}
	}
	return known
}

// noteUnknownKey is the only unknown-key policy, including membership.
// v1 ignores keys that are not in known. A later revision can panic here
// without changing resolve's walk. known is the registry key set.
func noteUnknownKey(key string, known map[string]struct{}) {
	if _, ok := known[key]; ok {
		return
	}
	// v1: ignore
}

// checkLogLevel validates only the effective level. Empty string is legal.
// Comparison is exact and case-sensitive. This is the only path that wraps
// ErrInvalidLogLevel, so a row validator cannot steal the sentinel.
func checkLogLevel(level string) error {
	switch level {
	case "", "debug", "info", "warn", "error":
		return nil
	default:
		return fmt.Errorf("config: log level %q: %w", level, ErrInvalidLogLevel)
	}
}
