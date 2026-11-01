// Package config computes an effective configuration by combining ordered
// layers of in-memory settings. Higher-precedence layers override lower ones.
//
// The package performs no I/O: the layer names (defaults, file, env, flags)
// denote precedence only, and callers supply every layer's values.
package config

import (
	"fmt"
	"sort"
)

// Config is the effective configuration produced by Resolve.
//
// To add a field: add it here and add one matching entry to the fields table
// in fields.go. A test fails if the two drift out of sync.
type Config struct {
	Verbose    bool
	MaxRetries int
	LogLevel   string
	TimeoutSec int
}

// Resolve combines the four layers, given in ascending precedence order
// (defaults, file, env, flags), into the effective Config.
//
// A key present in a layer's map sets the field, even when the value is a
// zero value (false, 0, ""). Among the layers presenting a field, the
// highest-precedence one wins. A key absent from a layer leaves the value from
// lower layers in place; a field presented by no layer has its Go zero value.
// A nil map means every field is absent in that layer. The input maps are
// neither modified nor retained.
//
// Only the effective LogLevel is validated. If it is not one of "debug",
// "info", "warn", "error" or "", Resolve returns the zero Config and an error
// satisfying errors.Is(err, ErrInvalidLogLevel).
//
// Resolve panics on programmer errors: a present value whose dynamic type is
// not exactly the field's type (including an untyped nil), in any layer, even
// if a higher layer overrides it; or a key that names no Config field.
func Resolve(defaults, file, env, flags map[string]any) (Config, error) {
	return resolve([]layer{
		{name: "defaults", values: defaults},
		{name: "file", values: file},
		{name: "env", values: env},
		{name: "flags", values: flags},
	})
}

// layer is one named source of settings. Its name is used only in panic
// messages.
type layer struct {
	name   string
	values map[string]any // nil means every field is absent
}

// resolve applies layers in ascending precedence order, so the last present
// value for each field wins, then validates the effective result. It does not
// depend on the number of layers.
func resolve(layers []layer) (Config, error) {
	var c Config
	for _, l := range layers {
		matched := 0
		for _, f := range fields {
			v, present := l.values[f.key]
			if !present {
				continue
			}
			matched++
			if !f.assign(&c, v) {
				panic(fmt.Sprintf("config: layer %q: key %q: want %s, got %T (%v)",
					l.name, f.key, f.typeName, v, v))
			}
		}
		if matched != len(l.values) {
			rejectUnknownKeys(l)
		}
	}
	if err := validate(c); err != nil {
		return Config{}, err
	}
	return c, nil
}

// rejectUnknownKeys is the single policy point for keys that name no Config
// field. Such keys are programmer errors (typically typos like "Loglevel")
// and panic, naming the layer and the lexicographically first unknown key so
// the message is deterministic. Change only this function to alter the policy.
func rejectUnknownKeys(l layer) {
	var unknown []string
	for k := range l.values {
		if _, ok := fieldIndex[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return
	}
	sort.Strings(unknown)
	panic(fmt.Sprintf("config: layer %q: unknown key %q", l.name, unknown[0]))
}
