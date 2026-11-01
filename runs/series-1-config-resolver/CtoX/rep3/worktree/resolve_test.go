package config

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

var layerNames = []string{"defaults", "file", "env", "flags"}

func resolve4(t *testing.T, ls [4]map[string]any) (Config, error) {
	t.Helper()
	return Resolve(ls[0], ls[1], ls[2], ls[3])
}

func mustPanic(t *testing.T, contains []string, fn func()) {
	t.Helper()
	defer func() {
		t.Helper()
		r := recover()
		if r == nil {
			t.Fatal("expected panic, got none")
		}
		msg := fmt.Sprint(r)
		for _, s := range contains {
			if !strings.Contains(msg, s) {
				t.Errorf("panic %q does not contain %q", msg, s)
			}
		}
	}()
	fn()
}

func TestAllNilGivesZero(t *testing.T) {
	got, err := Resolve(nil, nil, nil, nil)
	if err != nil || got != (Config{}) {
		t.Fatalf("got %+v, %v; want zero, nil", got, err)
	}
	got, err = Resolve(map[string]any{}, map[string]any{}, map[string]any{}, map[string]any{})
	if err != nil || got != (Config{}) {
		t.Fatalf("empty maps: got %+v, %v; want zero, nil", got, err)
	}
}

func TestFieldsFromDifferentLayers(t *testing.T) {
	got, err := Resolve(
		map[string]any{"Verbose": true},
		map[string]any{"MaxRetries": 7},
		map[string]any{"LogLevel": "error"},
		map[string]any{"TimeoutSec": 9})
	if err != nil {
		t.Fatal(err)
	}
	want := Config{Verbose: true, MaxRetries: 7, LogLevel: "error", TimeoutSec: 9}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// TestPrecedenceAllSubsets presents each field in every subset of layers and
// checks the highest presenting layer wins (or zero if none presents it).
func TestPrecedenceAllSubsets(t *testing.T) {
	// Distinct per-layer values; index 0..3 = layer. Includes zero values.
	values := map[string][4]any{
		"Verbose":    {true, false, true, false},
		"MaxRetries": {3, 0, 5, 0},
		"LogLevel":   {"info", "", "warn", "debug"},
		"TimeoutSec": {10, 20, 0, 40},
	}
	for key, vals := range values {
		for mask := 0; mask < 16; mask++ {
			var ls [4]map[string]any
			var want any
			for i := range 4 {
				if mask&(1<<i) != 0 {
					ls[i] = map[string]any{key: vals[i]}
					want = vals[i]
				}
			}
			got, err := resolve4(t, ls)
			if err != nil {
				t.Fatalf("%s mask %04b: %v", key, mask, err)
			}
			gotV := reflect.ValueOf(got).FieldByName(key)
			if want == nil {
				if !gotV.IsZero() {
					t.Errorf("%s mask %04b: got %v, want zero", key, mask, gotV)
				}
				continue
			}
			if gotV.Interface() != want {
				t.Errorf("%s mask %04b: got %v, want %v", key, mask, gotV, want)
			}
		}
	}
}

func TestLegalLogLevels(t *testing.T) {
	for _, lvl := range []string{"", "debug", "info", "warn", "error"} {
		got, err := Resolve(map[string]any{"LogLevel": lvl}, nil, nil, nil)
		if err != nil {
			t.Errorf("%q: unexpected error %v", lvl, err)
		}
		if got.LogLevel != lvl {
			t.Errorf("%q: got %q", lvl, got.LogLevel)
		}
	}
}

func TestIllegalLogLevels(t *testing.T) {
	for _, lvl := range []string{"trace", "INFO", " info", "warning"} {
		got, err := Resolve(map[string]any{"Verbose": true}, nil, map[string]any{"LogLevel": lvl}, nil)
		if !errors.Is(err, ErrInvalidLogLevel) {
			t.Errorf("%q: err = %v, want ErrInvalidLogLevel", lvl, err)
			continue
		}
		if got != (Config{}) {
			t.Errorf("%q: got %+v, want zero Config on error", lvl, got)
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("%q", lvl)) {
			t.Errorf("%q: error %q does not name the value", lvl, err)
		}
	}
}

func TestOverriddenInvalidLevelNotValidated(t *testing.T) {
	for _, over := range []string{"", "info"} {
		got, err := Resolve(map[string]any{"LogLevel": "trace"}, nil, nil, map[string]any{"LogLevel": over})
		if err != nil {
			t.Errorf("override %q: unexpected error %v", over, err)
		}
		if got.LogLevel != over {
			t.Errorf("override %q: got %q", over, got.LogLevel)
		}
	}
}

func TestInvalidDefaultLevelNotOverridden(t *testing.T) {
	_, err := Resolve(map[string]any{"LogLevel": "bogus"}, map[string]any{"Verbose": true}, nil, nil)
	if !errors.Is(err, ErrInvalidLogLevel) {
		t.Fatalf("err = %v, want ErrInvalidLogLevel", err)
	}
}

func TestOtherFieldsNotValidated(t *testing.T) {
	got, err := Resolve(map[string]any{"MaxRetries": -1, "TimeoutSec": -5}, nil, nil, nil)
	if err != nil || got.MaxRetries != -1 || got.TimeoutSec != -5 {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestWrongTypePanics(t *testing.T) {
	type level string
	cases := []struct {
		key string
		val any
	}{
		{"Verbose", "true"},
		{"Verbose", 1},
		{"MaxRetries", int64(3)},
		{"MaxRetries", 3.0},
		{"MaxRetries", uint(3)},
		{"LogLevel", 5},
		{"LogLevel", level("info")},
		{"TimeoutSec", "30"},
		{"TimeoutSec", nil},
	}
	for _, tc := range cases {
		for li := range 4 {
			var ls [4]map[string]any
			ls[li] = map[string]any{tc.key: tc.val}
			mustPanic(t, []string{layerNames[li], tc.key}, func() { _, _ = resolve4(t, ls) })
		}
	}
}

func TestWrongTypeOverriddenStillPanics(t *testing.T) {
	mustPanic(t, []string{"defaults", "MaxRetries"}, func() {
		_, _ = Resolve(map[string]any{"MaxRetries": "3"}, nil, nil, map[string]any{"MaxRetries": 3})
	})
}

func TestUnknownKeyPanics(t *testing.T) {
	mustPanic(t, []string{"env", "Loglevel"}, func() {
		_, _ = Resolve(nil, nil, map[string]any{"Loglevel": "info", "Verbose": true}, nil)
	})
	mustPanic(t, []string{"flags", `"Aaa"`}, func() {
		_, _ = Resolve(nil, nil, nil, map[string]any{"Zzz": 1, "Aaa": 2})
	})
}

func TestInputsNotMutated(t *testing.T) {
	mk := func() [4]map[string]any {
		return [4]map[string]any{
			{"Verbose": true, "MaxRetries": 1, "LogLevel": "info", "TimeoutSec": 2},
			{"MaxRetries": 0},
			{"LogLevel": ""},
			{"Verbose": false},
		}
	}
	in, orig := mk(), mk()
	if _, err := resolve4(t, in); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, orig) {
		t.Fatalf("inputs mutated: %v vs %v", in, orig)
	}
}

// TestFieldTableMatchesConfig guards against Config and the fields table
// drifting apart as fields are added.
func TestFieldTableMatchesConfig(t *testing.T) {
	ct := reflect.TypeFor[Config]()
	if ct.NumField() != len(fields) {
		t.Fatalf("Config has %d fields, table has %d", ct.NumField(), len(fields))
	}
	for _, f := range fields {
		sf, ok := ct.FieldByName(f.key)
		if !ok {
			t.Errorf("table key %q is not a Config field", f.key)
			continue
		}
		if sf.Type.String() != f.typeName {
			t.Errorf("key %q: table type %s, struct type %s", f.key, f.typeName, sf.Type)
		}
		var c Config
		v := reflect.New(sf.Type).Elem().Interface()
		if !f.assign(&c, v) {
			t.Errorf("key %q: assign rejects zero %s", f.key, sf.Type)
		}
	}
	if len(fieldIndex) != len(fields) {
		t.Errorf("duplicate keys in fields table")
	}
}
