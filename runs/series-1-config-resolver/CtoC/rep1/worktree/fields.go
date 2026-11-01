package config

import (
	"fmt"
	"slices"
)

// field describes one Config field and how a layer value is applied to it.
type field struct {
	name  string                 // layer map key, e.g. "LogLevel"
	apply func(c *Config, v any) // type-checks v and stores it; panics on wrong type
}

// bind builds a field whose layer value must have exactly Go type T.
func bind[T any](name string, ptr func(*Config) *T) field {
	return field{
		name: name,
		apply: func(c *Config, v any) {
			t, ok := v.(T)
			if !ok {
				panic(fmt.Sprintf("config: key %q: value %#v has type %T, want %T", name, v, v, *new(T)))
			}
			*ptr(c) = t
		},
	}
}

// fields is the single table of resolvable Config fields. Adding a field to
// Config requires adding one entry here. It is read-only after init.
var fields = []field{
	bind("Verbose", func(c *Config) *bool { return &c.Verbose }),
	bind("MaxRetries", func(c *Config) *int { return &c.MaxRetries }),
	bind("LogLevel", func(c *Config) *string { return &c.LogLevel }),
	bind("TimeoutSec", func(c *Config) *int { return &c.TimeoutSec }),
}

// knownKey reports whether key names a field in the fields table.
func knownKey(key string) bool {
	for _, f := range fields {
		if f.name == key {
			return true
		}
	}
	return false
}

// checkKeys panics if layer contains a key that is not a known field name.
func checkKeys(layer map[string]any) {
	matched := 0
	for _, f := range fields {
		if _, ok := layer[f.name]; ok {
			matched++
		}
	}
	if matched == len(layer) {
		return
	}
	var unknown []string
	for k := range layer {
		if !knownKey(k) {
			unknown = append(unknown, k)
		}
	}
	slices.Sort(unknown)
	panic(fmt.Sprintf("config: unknown key(s) %q", unknown))
}
