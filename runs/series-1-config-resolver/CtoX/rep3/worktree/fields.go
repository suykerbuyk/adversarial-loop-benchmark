package config

import "reflect"

// field describes how one layer key maps onto a Config field.
type field struct {
	key      string                      // map key, e.g. "MaxRetries"
	typeName string                      // expected Go type, for panic messages
	assign   func(c *Config, v any) bool // stores v if its type is exactly right; reports success
}

// fieldOf builds a descriptor whose expected type is tied at compile time to
// the type of the Config field returned by ptr.
func fieldOf[T any](key string, ptr func(*Config) *T) field {
	return field{
		key:      key,
		typeName: reflect.TypeFor[T]().String(),
		assign: func(c *Config, v any) bool {
			t, ok := v.(T)
			if ok {
				*ptr(c) = t
			}
			return ok
		},
	}
}

// fields lists every Config field in struct order. Keep in sync with Config.
var fields = []field{
	fieldOf("Verbose", func(c *Config) *bool { return &c.Verbose }),
	fieldOf("MaxRetries", func(c *Config) *int { return &c.MaxRetries }),
	fieldOf("LogLevel", func(c *Config) *string { return &c.LogLevel }),
	fieldOf("TimeoutSec", func(c *Config) *int { return &c.TimeoutSec }),
}

// fieldIndex maps each legal key to its position in fields.
var fieldIndex = func() map[string]int {
	m := make(map[string]int, len(fields))
	for i, f := range fields {
		m[f.key] = i
	}
	return m
}()
