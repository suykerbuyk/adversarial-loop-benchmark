package config

import (
	"errors"
	"testing"
)

// Public acceptance tests for the layered config resolver. Shipped with the task;
// the implementor sees these. They exercise the eight cases in the cell-facing spec.

func TestDefaultsOnly(t *testing.T) {
	got, err := Resolve(
		map[string]any{"Verbose": true, "MaxRetries": 5, "LogLevel": "warn", "TimeoutSec": 30},
		nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := Config{Verbose: true, MaxRetries: 5, LogLevel: "warn", TimeoutSec: 30}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestHigherNonZeroWins(t *testing.T) {
	got, err := Resolve(
		map[string]any{"LogLevel": "info"},
		map[string]any{"LogLevel": "debug"},
		nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.LogLevel != "debug" {
		t.Fatalf("LogLevel = %q, want %q", got.LogLevel, "debug")
	}
}

func TestPresentFalseOverridesTrue(t *testing.T) {
	got, err := Resolve(
		map[string]any{"Verbose": true},
		nil, nil,
		map[string]any{"Verbose": false})
	if err != nil {
		t.Fatal(err)
	}
	if got.Verbose != false {
		t.Fatalf("Verbose = %v, want false", got.Verbose)
	}
}

func TestPresentZeroIntOverrides(t *testing.T) {
	got, err := Resolve(
		map[string]any{"MaxRetries": 3},
		nil,
		map[string]any{"MaxRetries": 0},
		nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxRetries != 0 {
		t.Fatalf("MaxRetries = %d, want 0", got.MaxRetries)
	}
}

func TestPresentEmptyStringOverrides(t *testing.T) {
	got, err := Resolve(
		map[string]any{"LogLevel": "info"},
		map[string]any{"LogLevel": ""},
		nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.LogLevel != "" {
		t.Fatalf("LogLevel = %q, want empty", got.LogLevel)
	}
}

func TestAbsentPassesThrough(t *testing.T) {
	got, err := Resolve(
		nil,
		map[string]any{"LogLevel": "warn"},
		nil,
		map[string]any{"Verbose": true}) // flags is absent for LogLevel
	if err != nil {
		t.Fatal(err)
	}
	if got.LogLevel != "warn" {
		t.Fatalf("LogLevel = %q, want %q", got.LogLevel, "warn")
	}
}

func TestAllFourLayersPrecedence(t *testing.T) {
	got, err := Resolve(
		map[string]any{"TimeoutSec": 1},
		map[string]any{"TimeoutSec": 2},
		map[string]any{"TimeoutSec": 3},
		map[string]any{"TimeoutSec": 4})
	if err != nil {
		t.Fatal(err)
	}
	if got.TimeoutSec != 4 {
		t.Fatalf("TimeoutSec = %d, want 4", got.TimeoutSec)
	}
}

func TestInvalidEffectiveLogLevel(t *testing.T) {
	_, err := Resolve(
		nil, nil, nil,
		map[string]any{"LogLevel": "trace"})
	if !errors.Is(err, ErrInvalidLogLevel) {
		t.Fatalf("err = %v, want ErrInvalidLogLevel", err)
	}
}
