package config

import (
	"errors"
	"fmt"
)

// ErrInvalidLogLevel is returned when the effective LogLevel is not one of
// "debug", "info", "warn", "error", or "".
var ErrInvalidLogLevel = errors.New("config: invalid log level")

// Config is the resolved configuration. Unpresented fields keep the Go zero value.
type Config struct {
	Verbose    bool
	MaxRetries int
	LogLevel   string
	TimeoutSec int
}

type fieldID int

const (
	fieldVerbose fieldID = iota
	fieldMaxRetries
	fieldLogLevel
	fieldTimeoutSec
)

type kind int

const (
	kindBool kind = iota
	kindInt
	kindString
)

type fieldDesc struct {
	id   fieldID
	key  string
	kind kind
}

// fieldTable is the allow-list of legal keys. Iteration order is independent of
// layer precedence. Adding a field is one entry plus a Config member and an
// assignment arm in assign.
var fieldTable = []fieldDesc{
	{fieldVerbose, "Verbose", kindBool},
	{fieldMaxRetries, "MaxRetries", kindInt},
	{fieldLogLevel, "LogLevel", kindString},
	{fieldTimeoutSec, "TimeoutSec", kindInt},
}

type layer struct {
	name string
	vals map[string]any
}

// Resolve combines four layers in ascending precedence: defaults, file, env, flags.
// A key present in a layer sets that field, including zero values (false, 0, "").
// A key absent from a layer leaves the field unchanged. A present value of the
// wrong Go type panics. An unknown key panics. Only the effective LogLevel is
// validated.
func Resolve(defaults, file, env, flags map[string]any) (Config, error) {
	layers := [4]layer{
		{"defaults", defaults},
		{"file", file},
		{"env", env},
		{"flags", flags},
	}

	known := make(map[string]struct{}, len(fieldTable))
	for _, f := range fieldTable {
		known[f.key] = struct{}{}
	}
	for _, l := range layers {
		if l.vals == nil {
			continue
		}
		for key := range l.vals {
			if _, ok := known[key]; !ok {
				panic(fmt.Sprintf("config: unknown field %q in layer %q", key, l.name))
			}
		}
	}

	var cfg Config
	for _, f := range fieldTable {
		for _, l := range layers {
			// A nil map has no keys. Presence is key existence, so a stored
			// zero value (false, 0, "") still overwrites a lower layer.
			if l.vals == nil {
				continue
			}
			v, ok := l.vals[f.key]
			if !ok {
				continue
			}
			cfg = assign(cfg, f, coerce(f, v, l))
		}
	}

	if !legalLogLevel(cfg.LogLevel) {
		return Config{}, fmt.Errorf("%w: %q", ErrInvalidLogLevel, cfg.LogLevel)
	}
	return cfg, nil
}

func coerce(f fieldDesc, v any, l layer) any {
	switch f.kind {
	case kindBool:
		b, ok := v.(bool)
		if !ok {
			panic(typeMismatch(f.key, l.name, v, "bool"))
		}
		return b
	case kindInt:
		n, ok := v.(int)
		if !ok {
			panic(typeMismatch(f.key, l.name, v, "int"))
		}
		return n
	case kindString:
		s, ok := v.(string)
		if !ok {
			panic(typeMismatch(f.key, l.name, v, "string"))
		}
		return s
	default:
		panic(fmt.Sprintf("config: field %q: unknown kind %d", f.key, f.kind))
	}
}

func typeMismatch(key, layerName string, v any, want string) string {
	return fmt.Sprintf("config: field %q in layer %q: got %T, want %s", key, layerName, v, want)
}

func assign(cfg Config, f fieldDesc, v any) Config {
	switch f.id {
	case fieldVerbose:
		cfg.Verbose = v.(bool)
	case fieldMaxRetries:
		cfg.MaxRetries = v.(int)
	case fieldLogLevel:
		cfg.LogLevel = v.(string)
	case fieldTimeoutSec:
		cfg.TimeoutSec = v.(int)
	default:
		panic(fmt.Sprintf("config: unassigned field %q", f.key))
	}
	return cfg
}

func legalLogLevel(s string) bool {
	switch s {
	case "", "debug", "info", "warn", "error":
		return true
	default:
		return false
	}
}
