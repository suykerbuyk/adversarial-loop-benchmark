package config

import (
	"fmt"
	"reflect"
)

// field binds a layer map key to a Config field.
type field struct {
	key string
	// set type-checks v and assigns it to the bound field of c. It panics if
	// v does not have exactly the field's Go type. layerName is used only in
	// the panic message.
	set func(c *Config, layerName string, v any)
}

// bind returns a field for key whose values must have exact type T and are
// stored at the location returned by ptr.
func bind[T any](key string, ptr func(*Config) *T) field {
	return field{
		key: key,
		set: func(c *Config, layerName string, v any) {
			t, ok := v.(T)
			if !ok {
				panic(fmt.Sprintf("config: layer %q: key %q: value %#v has type %T, want %v",
					layerName, key, v, v, reflect.TypeFor[T]()))
			}
			*ptr(c) = t
		},
	}
}

const logLevelKey = "LogLevel"

// fields lists every configurable field. To add a field, add it to Config
// and add one bind entry here.
var fields = []field{
	bind("Verbose", func(c *Config) *bool { return &c.Verbose }),
	bind("MaxRetries", func(c *Config) *int { return &c.MaxRetries }),
	bind(logLevelKey, func(c *Config) *string { return &c.LogLevel }),
	bind("TimeoutSec", func(c *Config) *int { return &c.TimeoutSec }),
}

var fieldsByKey = indexFields(fields)

// indexFields maps each field key to its field, panicking on duplicates.
func indexFields(fs []field) map[string]field {
	m := make(map[string]field, len(fs))
	for _, f := range fs {
		if _, dup := m[f.key]; dup {
			panic(fmt.Sprintf("config: duplicate field key %q", f.key))
		}
		m[f.key] = f
	}
	return m
}
