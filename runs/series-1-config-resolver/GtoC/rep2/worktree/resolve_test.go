package config

import (
	"errors"
	"maps"
	"reflect"
	"testing"
)

func TestNoLayerPresentsAnything(t *testing.T) {
	got, err := Resolve(nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != (Config{}) {
		t.Fatalf("got %+v, want zero Config", got)
	}
}

func TestEachLegalLogLevelFromFlags(t *testing.T) {
	for level := range legalLogLevels {
		got, err := Resolve(nil, nil, nil, map[string]any{"LogLevel": level})
		if err != nil {
			t.Fatalf("LogLevel %q: unexpected error: %v", level, err)
		}
		if got.LogLevel != level {
			t.Fatalf("LogLevel = %q, want %q", got.LogLevel, level)
		}
	}
}

func TestIllegalEffectiveLogLevelReturnsMergedConfig(t *testing.T) {
	for _, level := range []string{"Trace", "debug ", "fatal"} {
		got, err := Resolve(nil, nil, nil, map[string]any{"LogLevel": level, "Verbose": true})
		if !errors.Is(err, ErrInvalidLogLevel) {
			t.Fatalf("LogLevel %q: err = %v, want ErrInvalidLogLevel", level, err)
		}
		if got.LogLevel != level {
			t.Fatalf("LogLevel = %q, want %q", got.LogLevel, level)
		}
		if !got.Verbose {
			t.Fatalf("LogLevel %q: Verbose = false, want true (merged config returned with error)", level)
		}
	}
}

func TestIllegalLowerLevelOverriddenByLegalHigher(t *testing.T) {
	got, err := Resolve(
		map[string]any{"LogLevel": "trace"},
		map[string]any{"LogLevel": "warn"},
		nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.LogLevel != "warn" {
		t.Fatalf("LogLevel = %q, want %q", got.LogLevel, "warn")
	}
}

func TestIllegalLowerLevelRemainsWhenHigherOmitsKey(t *testing.T) {
	got, err := Resolve(
		map[string]any{"LogLevel": "trace"},
		nil, nil,
		map[string]any{"Verbose": true})
	if !errors.Is(err, ErrInvalidLogLevel) {
		t.Fatalf("err = %v, want ErrInvalidLogLevel", err)
	}
	if got.LogLevel != "trace" || !got.Verbose {
		t.Fatalf("got %+v, want LogLevel=trace Verbose=true", got)
	}
}

func TestZeroValueOverrideFromHighestLayer(t *testing.T) {
	got, err := Resolve(
		map[string]any{"Verbose": true, "MaxRetries": 3, "TimeoutSec": 30},
		nil, nil,
		map[string]any{"Verbose": false, "MaxRetries": 0, "TimeoutSec": 0})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := Config{Verbose: false, MaxRetries: 0, TimeoutSec: 0}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestMidLayerWinWhenHigherAbsent(t *testing.T) {
	got, err := Resolve(
		nil,
		map[string]any{"LogLevel": "info"},
		map[string]any{"TimeoutSec": 9},
		nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.TimeoutSec != 9 {
		t.Fatalf("TimeoutSec = %d, want 9", got.TimeoutSec)
	}
	if got.LogLevel != "info" {
		t.Fatalf("LogLevel = %q, want %q", got.LogLevel, "info")
	}
}

func TestMiddleZeroDoesNotLeakWhenHigherAbsent(t *testing.T) {
	got, err := Resolve(
		map[string]any{"MaxRetries": 4, "Verbose": true, "TimeoutSec": 8},
		map[string]any{"MaxRetries": 0, "Verbose": false},
		nil,
		map[string]any{"TimeoutSec": 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.MaxRetries != 0 {
		t.Fatalf("MaxRetries = %d, want 0", got.MaxRetries)
	}
	if got.Verbose {
		t.Fatal("Verbose = true, want false")
	}
	if got.TimeoutSec != 1 {
		t.Fatalf("TimeoutSec = %d, want 1", got.TimeoutSec)
	}
}

func TestEmptyStringOverridesIllegalLowerLevel(t *testing.T) {
	got, err := Resolve(
		map[string]any{"LogLevel": "trace"},
		nil, nil,
		map[string]any{"LogLevel": ""})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.LogLevel != "" {
		t.Fatalf("LogLevel = %q, want empty", got.LogLevel)
	}
}

func TestUnknownKeyPanics(t *testing.T) {
	for _, key := range []string{"verbose", "Extra"} {
		assertPanics(t, func() {
			_, _ = Resolve(nil, nil, nil, map[string]any{key: true})
		})
	}
}

func TestWrongTypePanics(t *testing.T) {
	cases := []map[string]any{
		{"Verbose": "yes"},
		{"MaxRetries": int64(3)},
		{"LogLevel": []byte("info")},
		{"TimeoutSec": float64(1)},
		{"Verbose": nil},
	}
	for _, layer := range cases {
		assertPanics(t, func() {
			_, _ = Resolve(nil, nil, nil, layer)
		})
	}
}

func TestUnknownKeyInLowerLayerPanics(t *testing.T) {
	assertPanics(t, func() {
		_, _ = Resolve(
			map[string]any{"Extra": 1},
			nil, nil,
			map[string]any{"Verbose": true, "LogLevel": "info"})
	})
}

func TestWrongTypeInLowerLayerPanicsDespiteHigherOverride(t *testing.T) {
	assertPanics(t, func() {
		_, _ = Resolve(
			map[string]any{"MaxRetries": "3"},
			nil, nil,
			map[string]any{"MaxRetries": 1})
	})
}

func TestInputMapsAreNotMutated(t *testing.T) {
	defaults := map[string]any{"Verbose": true, "MaxRetries": 2, "LogLevel": "info", "TimeoutSec": 5}
	file := map[string]any{"LogLevel": "debug"}
	env := map[string]any{"MaxRetries": 0}
	flags := map[string]any{"Verbose": false}
	before := []map[string]any{
		maps.Clone(defaults),
		maps.Clone(file),
		maps.Clone(env),
		maps.Clone(flags),
	}
	if _, err := Resolve(defaults, file, env, flags); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	after := []map[string]any{defaults, file, env, flags}
	for i := range before {
		if !maps.Equal(before[i], after[i]) {
			t.Fatalf("layer %d mutated: before %v after %v", i, before[i], after[i])
		}
	}
}

func TestErrInvalidLogLevelStable(t *testing.T) {
	if ErrInvalidLogLevel == nil {
		t.Fatal("ErrInvalidLogLevel is nil")
	}
	sentinel := ErrInvalidLogLevel
	_, err := Resolve(nil, nil, nil, map[string]any{"LogLevel": "trace"})
	if err == sentinel {
		t.Fatal("validation error must wrap the sentinel, not be the sentinel itself")
	}
	if !errors.Is(err, sentinel) || !errors.Is(err, ErrInvalidLogLevel) {
		t.Fatalf("err = %v, want errors.Is ErrInvalidLogLevel", err)
	}
	if ErrInvalidLogLevel != sentinel {
		t.Fatal("ErrInvalidLogLevel changed identity")
	}
}

func TestCatalogCoversExportedConfigFields(t *testing.T) {
	typ := reflect.TypeOf(Config{})
	seen := map[string]int{}
	for _, spec := range fieldCatalog {
		seen[spec.key]++
		field, ok := typ.FieldByName(spec.key)
		if !ok {
			t.Fatalf("catalog key %q is not an exported Config field", spec.key)
		}
		if !field.IsExported() {
			t.Fatalf("catalog key %q is not exported", spec.key)
		}
		var want reflect.Kind
		switch spec.kind {
		case kindBool:
			want = reflect.Bool
		case kindInt:
			want = reflect.Int
		case kindString:
			want = reflect.String
		default:
			t.Fatalf("catalog key %q has unknown kind %d", spec.key, spec.kind)
		}
		if field.Type.Kind() != want {
			t.Fatalf("catalog key %q kind %v, field type %s", spec.key, spec.kind, field.Type)
		}
	}
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		if seen[field.Name] != 1 {
			t.Fatalf("exported field %s appears %d times in fieldCatalog, want 1", field.Name, seen[field.Name])
		}
	}
}

func TestNegativeIntsAccepted(t *testing.T) {
	got, err := Resolve(nil, nil, nil, map[string]any{"MaxRetries": -1, "TimeoutSec": -2, "LogLevel": "error"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.MaxRetries != -1 || got.TimeoutSec != -2 {
		t.Fatalf("got %+v, want negative ints preserved", got)
	}
}

func assertPanics(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	fn()
}
