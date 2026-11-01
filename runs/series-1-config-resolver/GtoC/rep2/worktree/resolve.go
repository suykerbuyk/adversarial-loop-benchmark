package config

import (
	"errors"
	"fmt"
)

// Config is the effective configuration after layer merge.
// Field order and names are fixed; tests compare with ==.
type Config struct {
	Verbose    bool
	MaxRetries int
	LogLevel   string
	TimeoutSec int
}

// ErrInvalidLogLevel is returned (via errors.Is) when the effective LogLevel
// is not one of the values accepted by validate.
// The legal set is legalLogLevels.
var ErrInvalidLogLevel = errors.New("config: invalid log level")

// Resolve merges defaults, file, env, and flags in increasing precedence.
// A nil map is an empty layer (every field absent).
// Unknown keys and wrong Go types panic. See the package comment.
// On success and on validation failure, the Config value is the merged
// effective configuration. Validation runs only on the effective LogLevel.
//
// Resolve does not synchronize. The caller must not mutate a map during Resolve.
func Resolve(defaults, file, env, flags map[string]any) (Config, error) {
	return resolve(defaults, file, env, flags)
}

type fieldKind int

const (
	kindBool fieldKind = iota
	kindInt
	kindString
)

type fieldSpec struct {
	key  string
	kind fieldKind
}

// fieldCatalog is the single source of field keys and kinds.
var fieldCatalog = []fieldSpec{
	{key: "Verbose", kind: kindBool},
	{key: "MaxRetries", kind: kindInt},
	{key: "LogLevel", kind: kindString},
	{key: "TimeoutSec", kind: kindInt},
}

var catalogByKey = indexCatalog(fieldCatalog)

func indexCatalog(catalog []fieldSpec) map[string]fieldSpec {
	out := make(map[string]fieldSpec, len(catalog))
	for _, spec := range catalog {
		out[spec.key] = spec
	}
	return out
}

// legalLogLevels is the only set of accepted effective LogLevel values.
var legalLogLevels = map[string]struct{}{
	"":      {},
	"debug": {},
	"info":  {},
	"warn":  {},
	"error": {},
}

// resolve walks layers from lowest to highest precedence.
// A later present key overwrites an earlier one, including with false, 0, or "".
// A key absent from a layer is not written, so the lower value remains.
func resolve(layers ...map[string]any) (Config, error) {
	var cfg Config
	for _, layer := range layers {
		if layer == nil {
			continue
		}
		for key, value := range layer {
			spec, ok := catalogByKey[key]
			if !ok {
				panic(fmt.Sprintf("config: unknown key %q", key))
			}
			apply(&cfg, spec, value)
		}
	}
	return cfg, validate(cfg)
}

// apply type-checks v from spec.kind and writes the matching Config field.
// A kind/type mismatch, or a catalog key with no write arm, panics.
func apply(dst *Config, spec fieldSpec, v any) {
	switch spec.kind {
	case kindBool:
		b, ok := v.(bool)
		if !ok {
			panic(fmt.Sprintf("config: key %q: got %T, want bool", spec.key, v))
		}
		switch spec.key {
		case "Verbose":
			dst.Verbose = b
		default:
			panic(fmt.Sprintf("config: key %q has no bool write arm", spec.key))
		}
	case kindInt:
		n, ok := v.(int)
		if !ok {
			panic(fmt.Sprintf("config: key %q: got %T, want int", spec.key, v))
		}
		switch spec.key {
		case "MaxRetries":
			dst.MaxRetries = n
		case "TimeoutSec":
			dst.TimeoutSec = n
		default:
			panic(fmt.Sprintf("config: key %q has no int write arm", spec.key))
		}
	case kindString:
		s, ok := v.(string)
		if !ok {
			panic(fmt.Sprintf("config: key %q: got %T, want string", spec.key, v))
		}
		switch spec.key {
		case "LogLevel":
			dst.LogLevel = s
		default:
			panic(fmt.Sprintf("config: key %q has no string write arm", spec.key))
		}
	default:
		panic(fmt.Sprintf("config: key %q: unknown kind %d", spec.key, spec.kind))
	}
}

func validate(cfg Config) error {
	if _, ok := legalLogLevels[cfg.LogLevel]; ok {
		return nil
	}
	return fmt.Errorf("%w: %q", ErrInvalidLogLevel, cfg.LogLevel)
}
