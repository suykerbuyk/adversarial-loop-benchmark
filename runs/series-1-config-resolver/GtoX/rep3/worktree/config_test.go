package config

import (
	"errors"
	"testing"
)

func TestAllLayersNil(t *testing.T) {
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
	if got.Verbose != true {
		t.Fatalf("Verbose = %v, want true", got.Verbose)
	}
	if got.MaxRetries != 0 || got.LogLevel != "" || got.TimeoutSec != 0 {
		t.Fatalf("absent fields = %+v, want zeros", got)
	}
}

func TestMiddleLayersAbsentDoNotBlockHigher(t *testing.T) {
	got, err := Resolve(
		map[string]any{"TimeoutSec": 9},
		nil,
		nil,
		map[string]any{"TimeoutSec": 12})
	if err != nil {
		t.Fatal(err)
	}
	if got.TimeoutSec != 12 {
		t.Fatalf("TimeoutSec = %d, want 12", got.TimeoutSec)
	}
}

func TestAbsentHigherLeavesLower(t *testing.T) {
	got, err := Resolve(
		map[string]any{"MaxRetries": 7},
		nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxRetries != 7 {
		t.Fatalf("MaxRetries = %d, want 7", got.MaxRetries)
	}
}

func TestOverriddenIllegalLogLevelNotValidated(t *testing.T) {
	got, err := Resolve(
		map[string]any{"LogLevel": "trace"},
		nil,
		map[string]any{"LogLevel": "info"},
		nil)
	if err != nil {
		t.Fatalf("overridden illegal level must not fail: %v", err)
	}
	if got.LogLevel != "info" {
		t.Fatalf("LogLevel = %q, want %q", got.LogLevel, "info")
	}
}

func TestEffectiveIllegalLogLevelFromHigherLayer(t *testing.T) {
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
		t.Run(level, func(t *testing.T) {
			got, err := Resolve(nil, nil, nil, map[string]any{"LogLevel": level})
			if err != nil {
				t.Fatalf("level %q: %v", level, err)
			}
			if got.LogLevel != level {
				t.Fatalf("LogLevel = %q, want %q", got.LogLevel, level)
			}
		})
	}
}

func TestIllegalLogLevels(t *testing.T) {
	levels := []string{"INFO", "warning", " debug", "trace", "Error", "warn "}
	for _, level := range levels {
		t.Run(level, func(t *testing.T) {
			_, err := Resolve(nil, nil, nil, map[string]any{"LogLevel": level})
			if !errors.Is(err, ErrInvalidLogLevel) {
				t.Fatalf("level %q: err = %v, want ErrInvalidLogLevel", level, err)
			}
		})
	}
}

func TestInvalidLogLevelErrorIsWrapped(t *testing.T) {
	_, err := Resolve(nil, nil, nil, map[string]any{"LogLevel": "trace"})
	if !errors.Is(err, ErrInvalidLogLevel) {
		t.Fatalf("err = %v, want ErrInvalidLogLevel", err)
	}
	if err == ErrInvalidLogLevel {
		t.Fatal("expected wrapped error, got bare sentinel")
	}
}

func TestUnknownKeyPanics(t *testing.T) {
	mustPanic(t, func() {
		_, _ = Resolve(map[string]any{"verbose": true}, nil, nil, nil)
	})
	mustPanic(t, func() {
		_, _ = Resolve(nil, map[string]any{"": "x"}, nil, nil)
	})
	mustPanic(t, func() {
		_, _ = Resolve(nil, nil, map[string]any{"Loglevel": "info"}, nil)
	})
}

func TestWrongTypePanics(t *testing.T) {
	cases := []struct {
		name  string
		layer map[string]any
	}{
		{"Verbose string", map[string]any{"Verbose": "true"}},
		{"MaxRetries int64", map[string]any{"MaxRetries": int64(3)}},
		{"LogLevel int", map[string]any{"LogLevel": 1}},
		{"TimeoutSec float64", map[string]any{"TimeoutSec": float64(10)}},
		{"Verbose nil", map[string]any{"Verbose": nil}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mustPanic(t, func() {
				_, _ = Resolve(tc.layer, nil, nil, nil)
			})
		})
	}
}

func TestMixedZeroValuesShowThrough(t *testing.T) {
	got, err := Resolve(
		map[string]any{
			"Verbose":    true,
			"MaxRetries": 4,
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

func TestEmptyMapIsAllAbsent(t *testing.T) {
	got, err := Resolve(
		map[string]any{"LogLevel": "error", "TimeoutSec": 8},
		map[string]any{},
		map[string]any{},
		map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if got.LogLevel != "error" || got.TimeoutSec != 8 {
		t.Fatalf("got %+v, want defaults to show through", got)
	}
}

func TestNegativeIntsAreAccepted(t *testing.T) {
	got, err := Resolve(
		nil, nil, nil,
		map[string]any{"MaxRetries": -1, "TimeoutSec": -5})
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxRetries != -1 || got.TimeoutSec != -5 {
		t.Fatalf("got %+v, want negative ints applied", got)
	}
}

func mustPanic(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	fn()
}
