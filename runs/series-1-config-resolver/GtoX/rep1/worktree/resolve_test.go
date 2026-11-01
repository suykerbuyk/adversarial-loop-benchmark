package config

import (
	"errors"
	"testing"
)

func TestFourNilMaps(t *testing.T) {
	got, err := Resolve(nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != (Config{}) {
		t.Fatalf("got %+v, want zero Config", got)
	}
}

func TestAbsentDoesNotRestoreLowerNonZero(t *testing.T) {
	got, err := Resolve(
		map[string]any{"MaxRetries": 3},
		nil,
		map[string]any{"MaxRetries": 0},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxRetries != 0 {
		t.Fatalf("MaxRetries = %d, want 0", got.MaxRetries)
	}
}

func TestLowerInvalidLogLevelOverridden(t *testing.T) {
	got, err := Resolve(
		map[string]any{"LogLevel": "trace"},
		nil, nil,
		map[string]any{"LogLevel": "error"},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.LogLevel != "error" {
		t.Fatalf("LogLevel = %q, want %q", got.LogLevel, "error")
	}
}

func TestHigherInvalidLogLevelOverridesLegal(t *testing.T) {
	_, err := Resolve(
		map[string]any{"LogLevel": "info"},
		nil,
		map[string]any{"LogLevel": "trace"},
		nil,
	)
	if !errors.Is(err, ErrInvalidLogLevel) {
		t.Fatalf("err = %v, want ErrInvalidLogLevel", err)
	}
}

func TestEachLegalLogLevel(t *testing.T) {
	for _, level := range []string{"", "debug", "info", "warn", "error"} {
		got, err := Resolve(nil, nil, nil, map[string]any{"LogLevel": level})
		if err != nil {
			t.Fatalf("LogLevel %q: unexpected error: %v", level, err)
		}
		if got.LogLevel != level {
			t.Fatalf("LogLevel = %q, want %q", got.LogLevel, level)
		}
	}
}

func TestCaseMismatchAndUnknownLevel(t *testing.T) {
	for _, level := range []string{"Debug", "warning"} {
		_, err := Resolve(nil, nil, nil, map[string]any{"LogLevel": level})
		if !errors.Is(err, ErrInvalidLogLevel) {
			t.Fatalf("LogLevel %q: err = %v, want ErrInvalidLogLevel", level, err)
		}
	}
}

func TestMixedFieldsAcrossLayers(t *testing.T) {
	got, err := Resolve(
		map[string]any{"Verbose": true, "MaxRetries": 5, "LogLevel": "info", "TimeoutSec": 30},
		map[string]any{"LogLevel": "debug"},
		map[string]any{"MaxRetries": 0},
		map[string]any{"Verbose": false},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{Verbose: false, MaxRetries: 0, LogLevel: "debug", TimeoutSec: 30}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestWrongTypePanics(t *testing.T) {
	cases := []struct {
		name  string
		layer map[string]any
	}{
		{"Verbose string", map[string]any{"Verbose": "yes"}},
		{"MaxRetries int64", map[string]any{"MaxRetries": int64(1)}},
		{"LogLevel int", map[string]any{"LogLevel": 1}},
		{"TimeoutSec float64", map[string]any{"TimeoutSec": 1.5}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			_, _ = Resolve(tc.layer, nil, nil, nil)
		})
	}
}

func TestUnknownKeyPanics(t *testing.T) {
	for _, key := range []string{"verbose", "Timeout"} {
		t.Run(key, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			_, _ = Resolve(map[string]any{key: 1}, nil, nil, nil)
		})
	}
}

func TestWrongTypeInLowerLayerPanicsEvenIfHigherCorrect(t *testing.T) {
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
}

func TestValidationErrorReturnsZeroConfig(t *testing.T) {
	got, err := Resolve(
		map[string]any{"Verbose": true, "MaxRetries": 3, "TimeoutSec": 9},
		nil, nil,
		map[string]any{"LogLevel": "trace"},
	)
	if !errors.Is(err, ErrInvalidLogLevel) {
		t.Fatalf("err = %v, want ErrInvalidLogLevel", err)
	}
	if got != (Config{}) {
		t.Fatalf("got %+v, want zero Config", got)
	}
}

func TestCallerMapsNotMutated(t *testing.T) {
	defaults := map[string]any{"Verbose": true, "LogLevel": "info"}
	flags := map[string]any{"Verbose": false}
	beforeDefaults := copyMap(defaults)
	beforeFlags := copyMap(flags)

	_, err := Resolve(defaults, nil, nil, flags)
	if err != nil {
		t.Fatal(err)
	}
	if !mapsEqual(defaults, beforeDefaults) || !mapsEqual(flags, beforeFlags) {
		t.Fatal("Resolve mutated a caller map")
	}
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func mapsEqual(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
