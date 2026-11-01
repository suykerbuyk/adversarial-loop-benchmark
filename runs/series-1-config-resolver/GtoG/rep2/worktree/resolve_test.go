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

func TestPartialDefaultsLeaveRestZero(t *testing.T) {
	got, err := Resolve(
		map[string]any{"Verbose": true, "LogLevel": "info"},
		nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{Verbose: true, LogLevel: "info"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestFileWinsOverDefaultsWhenHigherAbsent(t *testing.T) {
	got, err := Resolve(
		map[string]any{"Verbose": false, "MaxRetries": 1, "LogLevel": "error", "TimeoutSec": 9},
		map[string]any{"Verbose": true, "MaxRetries": 8, "LogLevel": "debug", "TimeoutSec": 12},
		nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{Verbose: true, MaxRetries: 8, LogLevel: "debug", TimeoutSec: 12}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestNonAdjacentLayersFlagsWin(t *testing.T) {
	got, err := Resolve(
		map[string]any{"TimeoutSec": 10, "LogLevel": "info"},
		nil, nil,
		map[string]any{"TimeoutSec": 40})
	if err != nil {
		t.Fatal(err)
	}
	if got.TimeoutSec != 40 || got.LogLevel != "info" {
		t.Fatalf("got %+v, want TimeoutSec=40 LogLevel=info", got)
	}
}

func TestPerFieldHighestLayer(t *testing.T) {
	got, err := Resolve(
		map[string]any{"Verbose": false, "MaxRetries": 1, "LogLevel": "error", "TimeoutSec": 7},
		map[string]any{"LogLevel": "warn", "MaxRetries": 2},
		map[string]any{"MaxRetries": 9, "Verbose": true},
		map[string]any{"Verbose": false})
	if err != nil {
		t.Fatal(err)
	}
	want := Config{Verbose: false, MaxRetries: 9, LogLevel: "warn", TimeoutSec: 7}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestLowerIllegalLevelOverriddenByLegal(t *testing.T) {
	got, err := Resolve(
		map[string]any{"LogLevel": "trace"},
		map[string]any{"LogLevel": "info"},
		nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.LogLevel != "info" {
		t.Fatalf("LogLevel = %q, want info", got.LogLevel)
	}
}

func TestHigherIllegalLevelErrors(t *testing.T) {
	_, err := Resolve(
		map[string]any{"LogLevel": "info"},
		nil, nil,
		map[string]any{"LogLevel": "trace"})
	if !errors.Is(err, ErrInvalidLogLevel) {
		t.Fatalf("err = %v, want ErrInvalidLogLevel", err)
	}
}

func TestEmptyOverridesIllegal(t *testing.T) {
	got, err := Resolve(
		map[string]any{"LogLevel": "trace"},
		nil, nil,
		map[string]any{"LogLevel": ""})
	if err != nil {
		t.Fatal(err)
	}
	if got.LogLevel != "" {
		t.Fatalf("LogLevel = %q, want empty", got.LogLevel)
	}
}

func TestUnsetLogLevelWithOtherFields(t *testing.T) {
	got, err := Resolve(
		map[string]any{"Verbose": true, "MaxRetries": 2, "TimeoutSec": 15},
		nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{Verbose: true, MaxRetries: 2, TimeoutSec: 15}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestEachLegalLogLevel(t *testing.T) {
	for _, level := range []string{"", "debug", "info", "warn", "error"} {
		got, err := Resolve(nil, nil, nil, map[string]any{"LogLevel": level})
		if err != nil {
			t.Fatalf("level %q: %v", level, err)
		}
		if got.LogLevel != level {
			t.Fatalf("LogLevel = %q, want %q", got.LogLevel, level)
		}
	}
}

func TestIllegalLogLevelVariants(t *testing.T) {
	for _, level := range []string{"DEBUG", "Warn", " info", "trace", "warning", "Error"} {
		got, err := Resolve(nil, map[string]any{"LogLevel": level}, nil, nil)
		if !errors.Is(err, ErrInvalidLogLevel) {
			t.Fatalf("level %q: err = %v, want ErrInvalidLogLevel", level, err)
		}
		if got != (Config{}) {
			t.Fatalf("level %q: got %+v, want zero Config on error", level, got)
		}
	}
}

func TestPresentFalseWithNoLowerLayer(t *testing.T) {
	got, err := Resolve(nil, nil, nil, map[string]any{"Verbose": false})
	if err != nil {
		t.Fatal(err)
	}
	if got != (Config{}) {
		t.Fatalf("got %+v, want zero Config", got)
	}
}

func TestPresentZerosOnly(t *testing.T) {
	got, err := Resolve(
		nil,
		map[string]any{"MaxRetries": 0, "TimeoutSec": 0, "Verbose": false, "LogLevel": ""},
		nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != (Config{}) {
		t.Fatalf("got %+v, want zero Config", got)
	}
}

func TestNegativeIntegersAccepted(t *testing.T) {
	got, err := Resolve(
		nil, nil,
		map[string]any{"MaxRetries": -1, "TimeoutSec": -30, "LogLevel": "debug"},
		nil)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{MaxRetries: -1, TimeoutSec: -30, LogLevel: "debug"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestUnknownKeyPanics(t *testing.T) {
	layers := []struct {
		name string
		call func()
	}{
		{"defaults", func() { Resolve(map[string]any{"verbose": true}, nil, nil, nil) }},
		{"file", func() { Resolve(nil, map[string]any{"Extra": 1}, nil, nil) }},
		{"env", func() { Resolve(nil, nil, map[string]any{"Timeout": 1}, nil) }},
		{"flags", func() { Resolve(nil, nil, nil, map[string]any{"Loglevel": "info"}) }},
	}
	for _, tc := range layers {
		t.Run(tc.name, func(t *testing.T) {
			mustPanic(t, tc.call)
		})
	}
}

func TestWrongTypePanics(t *testing.T) {
	cases := []struct {
		name string
		call func()
	}{
		{"verbose string", func() { Resolve(map[string]any{"Verbose": "true"}, nil, nil, nil) }},
		{"maxretries int64", func() { Resolve(nil, map[string]any{"MaxRetries": int64(3)}, nil, nil) }},
		{"maxretries float64", func() { Resolve(nil, nil, map[string]any{"MaxRetries": float64(3)}, nil) }},
		{"loglevel bool", func() { Resolve(nil, nil, nil, map[string]any{"LogLevel": true}) }},
		{"timeout int32", func() { Resolve(map[string]any{"TimeoutSec": int32(5)}, nil, nil, nil) }},
		{"verbose typed nil", func() {
			var p *bool
			Resolve(nil, map[string]any{"Verbose": p}, nil, nil)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mustPanic(t, tc.call)
		})
	}
}

func TestExplicitTypedValuesAccepted(t *testing.T) {
	verbose := true
	retries := int(4)
	level := "warn"
	timeout := int(60)
	got, err := Resolve(
		map[string]any{"Verbose": verbose, "MaxRetries": retries, "LogLevel": level, "TimeoutSec": timeout},
		nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{Verbose: true, MaxRetries: 4, LogLevel: "warn", TimeoutSec: 60}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestInvalidLogLevelWrapsSentinel(t *testing.T) {
	got, err := Resolve(nil, nil, nil, map[string]any{"LogLevel": "trace"})
	if !errors.Is(err, ErrInvalidLogLevel) {
		t.Fatalf("err = %v, want ErrInvalidLogLevel", err)
	}
	if got != (Config{}) {
		t.Fatalf("got %+v, want zero Config", got)
	}
	if err.Error() == ErrInvalidLogLevel.Error() {
		t.Fatal("error should wrap the sentinel with the illegal value")
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
