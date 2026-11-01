package config

import (
	"errors"
	"testing"
)

func TestAllLayersNilOrEmpty(t *testing.T) {
	got, err := Resolve(nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != (Config{}) {
		t.Fatalf("got %+v, want zero Config", got)
	}

	got, err = Resolve(map[string]any{}, map[string]any{}, map[string]any{}, map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != (Config{}) {
		t.Fatalf("empty maps: got %+v, want zero Config", got)
	}
}

func TestMiddleLayerOnly(t *testing.T) {
	got, err := Resolve(
		nil,
		map[string]any{"Verbose": true, "TimeoutSec": 9},
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{Verbose: true, TimeoutSec: 9}
	if got != want {
		t.Fatalf("file only: got %+v, want %+v", got, want)
	}

	got, err = Resolve(
		nil,
		nil,
		map[string]any{"MaxRetries": 7, "LogLevel": "error"},
		map[string]any{"Verbose": false}, // omits MaxRetries and LogLevel
	)
	if err != nil {
		t.Fatal(err)
	}
	want = Config{MaxRetries: 7, LogLevel: "error"}
	if got != want {
		t.Fatalf("env shows through: got %+v, want %+v", got, want)
	}
}

func TestOverriddenInvalidLogLevelNotValidated(t *testing.T) {
	got, err := Resolve(
		map[string]any{"LogLevel": "trace"},
		nil,
		nil,
		map[string]any{"LogLevel": "error"},
	)
	if err != nil {
		t.Fatalf("overridden trace must not error: %v", err)
	}
	if got.LogLevel != "error" {
		t.Fatalf("LogLevel = %q, want error", got.LogLevel)
	}
}

func TestHigherInvalidLogLevel(t *testing.T) {
	_, err := Resolve(
		map[string]any{"LogLevel": "info"},
		nil,
		nil,
		map[string]any{"LogLevel": "trace"},
	)
	if !errors.Is(err, ErrInvalidLogLevel) {
		t.Fatalf("err = %v, want ErrInvalidLogLevel", err)
	}
	if err == ErrInvalidLogLevel {
		t.Fatal("want wrapped sentinel, not the sentinel itself")
	}
}

func TestLegalLogLevels(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "error", ""} {
		got, err := Resolve(nil, nil, nil, map[string]any{"LogLevel": level})
		if err != nil {
			t.Fatalf("level %q: unexpected error: %v", level, err)
		}
		if got.LogLevel != level {
			t.Fatalf("LogLevel = %q, want %q", got.LogLevel, level)
		}
	}
}

func TestCaseMismatchedLogLevel(t *testing.T) {
	_, err := Resolve(nil, nil, nil, map[string]any{"LogLevel": "INFO"})
	if !errors.Is(err, ErrInvalidLogLevel) {
		t.Fatalf("err = %v, want ErrInvalidLogLevel", err)
	}
}

func TestOtherFieldsNotValidatedWithBadLogLevel(t *testing.T) {
	got, err := Resolve(nil, nil, nil, map[string]any{
		"Verbose":    true,
		"MaxRetries": -1,
		"LogLevel":   "trace",
		"TimeoutSec": -5,
	})
	if !errors.Is(err, ErrInvalidLogLevel) {
		t.Fatalf("err = %v, want ErrInvalidLogLevel", err)
	}
	want := Config{Verbose: true, MaxRetries: -1, LogLevel: "trace", TimeoutSec: -5}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestWrongDynamicTypePanics(t *testing.T) {
	cases := []struct {
		name  string
		layer map[string]any
	}{
		{"verbose string", map[string]any{"Verbose": "yes"}},
		{"retries int64", map[string]any{"MaxRetries": int64(1)}},
		{"retries float64", map[string]any{"MaxRetries": float64(1)}},
		{"loglevel int", map[string]any{"LogLevel": 1}},
		{"timeout uint", map[string]any{"TimeoutSec": uint(1)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			_, _ = Resolve(nil, nil, nil, tc.layer)
		})
	}

	t.Run("wrong type on non-winning layer", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic")
			}
		}()
		_, _ = Resolve(
			map[string]any{"MaxRetries": int64(3)},
			nil, nil,
			map[string]any{"MaxRetries": 1},
		)
	})
}

func TestUnknownKeyPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	_, _ = Resolve(map[string]any{"Color": "blue"}, nil, nil, nil)
}

func TestIndependentFields(t *testing.T) {
	got, err := Resolve(
		map[string]any{"TimeoutSec": 1, "Verbose": true},
		map[string]any{"TimeoutSec": 2},
		map[string]any{"TimeoutSec": 3},
		map[string]any{"TimeoutSec": 4},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.TimeoutSec != 4 {
		t.Fatalf("TimeoutSec = %d, want 4", got.TimeoutSec)
	}
	if !got.Verbose {
		t.Fatal("Verbose from defaults should survive")
	}
	if got.MaxRetries != 0 || got.LogLevel != "" {
		t.Fatalf("untouched fields changed: %+v", got)
	}
}
