package config

import "fmt"

// field describes one Config field: its layer key and how to assign a value.
type field struct {
	key      string                      // map key; equals the Config field name
	typeName string                      // Go type name, for panic messages
	set      func(c *Config, v any) bool // assigns v if its dynamic type is exact; reports ok
}

// fieldOf builds a descriptor for the Config field selected by ptr. The type
// parameter T is fixed by ptr, so the descriptor cannot disagree with the
// struct field's type.
func fieldOf[T any](key string, ptr func(*Config) *T) field {
	var zero T
	return field{
		key:      key,
		typeName: fmt.Sprintf("%T", zero),
		set: func(c *Config, v any) bool {
			t, ok := v.(T) // exact dynamic type match; a nil interface fails
			if ok {
				*ptr(c) = t
			}
			return ok
		},
	}
}

// fields lists every Config field. Adding a field to Config requires adding
// its descriptor here (enforced by a test).
var fields = []field{
	fieldOf("Verbose", func(c *Config) *bool { return &c.Verbose }),
	fieldOf("MaxRetries", func(c *Config) *int { return &c.MaxRetries }),
	fieldOf("LogLevel", func(c *Config) *string { return &c.LogLevel }),
	fieldOf("TimeoutSec", func(c *Config) *int { return &c.TimeoutSec }),
}

// fieldIndex maps each legal key to its position in fields.
var fieldIndex = indexFields(fields)

func indexFields(fs []field) map[string]int {
	idx := make(map[string]int, len(fs))
	for i, f := range fs {
		if _, dup := idx[f.key]; dup {
			panic(fmt.Sprintf("config: duplicate field key %q", f.key))
		}
		idx[f.key] = i
	}
	return idx
}
