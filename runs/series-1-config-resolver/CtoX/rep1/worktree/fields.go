package config

// field describes one Config field: its layer-map key and how to assign it.
type field struct {
	key string                      // map key, e.g. "MaxRetries"
	set func(c *Config, v any) bool // asserts the exact Go type, assigns, reports ok
}

// fieldTable returns the descriptors for every Config field, in declaration order.
// It returns a new slice on each call, so no shared table exists that could be
// modified by accident.
//
// To add a field: add it to Config and add one row here. In each row, &c.X fixes
// both the destination and (by inference) the required dynamic type.
func fieldTable() []field {
	return []field{
		{"Verbose", func(c *Config, v any) bool { return assign(v, &c.Verbose) }},
		{"MaxRetries", func(c *Config, v any) bool { return assign(v, &c.MaxRetries) }},
		{"LogLevel", func(c *Config, v any) bool { return assign(v, &c.LogLevel) }},
		{"TimeoutSec", func(c *Config, v any) bool { return assign(v, &c.TimeoutSec) }},
	}
}

// assign stores v in *dst if v's dynamic type is exactly T, and reports whether it did.
// On failure *dst is left unchanged.
func assign[T any](v any, dst *T) bool {
	t, ok := v.(T)
	if ok {
		*dst = t
	}
	return ok
}
