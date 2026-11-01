package config

import (
	"errors"
	"testing"
)

func TestAllNilLayers(t *testing.T) {
	got, err := Resolve(nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != (Config{}) {
		t.Fatalf("got %+v, want zero Config", got)
	}
}

func TestAbsentFieldStaysZero(t *testing.T) {
	got, err := Resolve(
		map[string]any{"Verbose": true},
		nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxRetries != 0 || got.LogLevel != "" || got.TimeoutSec != 0 {
		t.Fatalf("absent fields not zero: %+v", got)
	}
	if !got.Verbose {
		t.Fatalf("Verbose = %v, want true", got.Verbose)
	}
}

func TestFlagsWinWhenMiddleLayersOmit(t *testing.T) {
	got, err := Resolve(
		map[string]any{"TimeoutSec": 10},
		nil,
		nil,
		map[string]any{"TimeoutSec": 40})
	if err != nil {
		t.Fatal(err)
	}
	if got.TimeoutSec != 40 {
		t.Fatalf("TimeoutSec = %d, want 40", got.TimeoutSec)
	}
}

func TestDefaultsRemainWhenHigherLayersOmit(t *testing.T) {
	got, err := Resolve(
		map[string]any{"LogLevel": "error", "MaxRetries": 2},
		nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.LogLevel != "error" || got.MaxRetries != 2 {
		t.Fatalf("got %+v", got)
	}
}

func TestOverriddenIllegalLogLevelNotValidated(t *testing.T) {
	got, err := Resolve(
		map[string]any{"LogLevel": "trace"},
		nil, nil,
		map[string]any{"LogLevel": "info"})
	if err != nil {
		t.Fatalf("overridden illegal level must not fail: %v", err)
	}
	if got.LogLevel != "info" {
		t.Fatalf("LogLevel = %q, want info", got.LogLevel)
	}
}

func TestEffectiveIllegalLogLevel(t *testing.T) {
	_, err := Resolve(
		map[string]any{"LogLevel": "info"},
		nil, nil,
		map[string]any{"LogLevel": "trace"})
	if !errors.Is(err, ErrInvalidLogLevel) {
		t.Fatalf("err = %v, want ErrInvalidLogLevel", err)
	}
}

func TestLegalLogLevels(t *testing.T) {
	levels := []string{"", "debug", "info", "warn", "error"}
	for _, level := range levels {
		got, err := Resolve(nil, nil, nil, map[string]any{"LogLevel": level})
		if err != nil {
			t.Fatalf("level %q: %v", level, err)
		}
		if got.LogLevel != level {
			t.Fatalf("LogLevel = %q, want %q", got.LogLevel, level)
		}
	}
}

func TestIllegalLogLevels(t *testing.T) {
	levels := []string{"INFO", "warning", " debug", "trace", "Error"}
	for _, level := range levels {
		cfg, err := Resolve(nil, nil, nil, map[string]any{"LogLevel": level})
		if !errors.Is(err, ErrInvalidLogLevel) {
			t.Fatalf("level %q: err = %v, want ErrInvalidLogLevel", level, err)
		}
		if cfg != (Config{}) {
			t.Fatalf("level %q: got %+v, want zero Config on error", level, cfg)
		}
	}
}

func TestUnknownKeyPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for unknown key")
		}
	}()
	_, _ = Resolve(map[string]any{"verbose": true}, nil, nil, nil)
}

func TestEmptyKeyPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for empty key")
		}
	}()
	_, _ = Resolve(nil, map[string]any{"": 1}, nil, nil)
}

func TestWrongTypesPanic(t *testing.T) {
	cases := []map[string]any{
		{"Verbose": "true"},
		{"MaxRetries": int64(3)},
		{"LogLevel": 1},
		{"TimeoutSec": float64(5)},
		{"Verbose": (*bool)(nil)},
	}
	for _, layer := range cases {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("expected panic for %#v", layer)
				}
			}()
			_, _ = Resolve(layer, nil, nil, nil)
		}()
	}
}

func TestWrappedSentinel(t *testing.T) {
	_, err := Resolve(nil, nil, nil, map[string]any{"LogLevel": "trace"})
	if !errors.Is(err, ErrInvalidLogLevel) {
		t.Fatalf("errors.Is failed: %v", err)
	}
	if err == ErrInvalidLogLevel {
		t.Fatal("expected wrapped error, got bare sentinel")
	}
}

func TestMixedZeroValuesShowThrough(t *testing.T) {
	got, err := Resolve(
		map[string]any{
			"Verbose":    true,
			"MaxRetries": 3,
			"LogLevel":   "warn",
			"TimeoutSec": 15,
		},
		map[string]any{
			"Verbose":    false,
			"MaxRetries": 0,
		},
		nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{Verbose: false, MaxRetries: 0, LogLevel: "warn", TimeoutSec: 15}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestNegativeIntsAccepted(t *testing.T) {
	got, err := Resolve(nil, nil, nil, map[string]any{
		"MaxRetries": -1,
		"TimeoutSec": -30,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxRetries != -1 || got.TimeoutSec != -30 {
		t.Fatalf("got %+v", got)
	}
}
