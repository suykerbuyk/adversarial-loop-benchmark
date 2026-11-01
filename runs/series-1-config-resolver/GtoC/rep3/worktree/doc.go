// Package config computes an effective configuration by combining ordered layers.
//
// The caller supplies every layer as a map. Resolve does not read the environment,
// files, or the command line. Layer names denote precedence only.
//
// Resolve documents the presence rule, the legal keys, the panic conditions, and
// LogLevel validation.
package config
