package config

import (
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
)

// mustPanic runs fn, fails the test if it does not panic, and returns the
// panic value rendered as a string.
func mustPanic(t *testing.T, fn func()) (msg string) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected panic, got none")
		}
		s, ok := r.(string)
		if !ok {
			t.Fatalf("panic value is %T, want string", r)
		}
		msg = s
	}()
	fn()
	return ""
}

func wantContains(t *testing.T, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			t.Errorf("%q does not contain %q", s, sub)
		}
	}
}

var layerNames = []string{"defaults", "file", "env", "flags"}

// resolveLayers calls Resolve with ls in precedence order.
func resolveLayers(ls [4]map[string]any) (Config, error) {
	return Resolve(ls[0], ls[1], ls[2], ls[3])
}

func TestZeroValueOverridesPerField(t *testing.T) {
	nonZero := map[string]any{"Verbose": true, "MaxRetries": 7, "LogLevel": "warn", "TimeoutSec": 9}
	zero := map[string]any{"Verbose": false, "MaxRetries": 0, "LogLevel": "", "TimeoutSec": 0}
	for key := range nonZero {
		for hi := 1; hi < 4; hi++ {
			for lo := 0; lo < hi; lo++ {
				var ls [4]map[string]any
				ls[lo] = map[string]any{key: nonZero[key]}
				ls[hi] = map[string]any{key: zero[key]}
				got, err := resolveLayers(ls)
				if err != nil {
					t.Fatalf("%s lo=%d hi=%d: %v", key, lo, hi, err)
				}
				if got != (Config{}) {
					t.Errorf("%s lo=%s hi=%s: got %+v, want zero", key, layerNames[lo], layerNames[hi], got)
				}
			}
		}
	}
}

func TestAbsentShowsThroughPerField(t *testing.T) {
	full := map[string]any{"Verbose": true, "MaxRetries": 7, "LogLevel": "warn", "TimeoutSec": 9}
	want := Config{Verbose: true, MaxRetries: 7, LogLevel: "warn", TimeoutSec: 9}
	for pos := 0; pos < 4; pos++ {
		var ls [4]map[string]any
		for i := range ls {
			ls[i] = map[string]any{}
		}
		ls[pos] = full
		got, err := resolveLayers(ls)
		if err != nil || got != want {
			t.Errorf("pos=%s: got %+v, %v; want %+v", layerNames[pos], got, err, want)
		}
	}
}

func TestMixedWinners(t *testing.T) {
	got, err := Resolve(
		map[string]any{"Verbose": true, "MaxRetries": 1, "LogLevel": "debug", "TimeoutSec": 1},
		map[string]any{"MaxRetries": 2, "LogLevel": "info"},
		map[string]any{"LogLevel": "error", "TimeoutSec": 3},
		map[string]any{"MaxRetries": -4},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{Verbose: true, MaxRetries: -4, LogLevel: "error", TimeoutSec: 3}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestEmptyLayers(t *testing.T) {
	for name, ls := range map[string][4]map[string]any{
		"nil":   {},
		"empty": {{}, {}, {}, {}},
	} {
		got, err := resolveLayers(ls)
		if err != nil || got != (Config{}) {
			t.Errorf("%s: got %+v, %v; want zero, nil", name, got, err)
		}
	}
}

func TestValidLogLevels(t *testing.T) {
	for _, lvl := range []string{"debug", "info", "warn", "error", ""} {
		got, err := Resolve(nil, nil, map[string]any{"LogLevel": lvl}, nil)
		if err != nil {
			t.Errorf("%q: unexpected error %v", lvl, err)
		}
		if got.LogLevel != lvl {
			t.Errorf("%q: got %q", lvl, got.LogLevel)
		}
	}
}

func TestInvalidLogLevels(t *testing.T) {
	for _, lvl := range []string{"trace", "INFO", " info", "warning"} {
		got, err := Resolve(map[string]any{"Verbose": true, "MaxRetries": 2}, nil, nil, map[string]any{"LogLevel": lvl})
		if !errors.Is(err, ErrInvalidLogLevel) {
			t.Errorf("%q: err = %v, want ErrInvalidLogLevel", lvl, err)
			continue
		}
		if got != (Config{}) {
			t.Errorf("%q: got %+v, want zero Config on error", lvl, got)
		}
		wantContains(t, err.Error(), lvl)
	}
}

func TestOverriddenInvalidLevelNotValidated(t *testing.T) {
	for _, over := range []string{"debug", ""} {
		got, err := Resolve(map[string]any{"LogLevel": "trace"}, map[string]any{"LogLevel": over}, nil, nil)
		if err != nil {
			t.Errorf("override %q: unexpected error %v", over, err)
		}
		if got.LogLevel != over {
			t.Errorf("override %q: got %q", over, got.LogLevel)
		}
	}
}

func TestInvalidLevelShowsThrough(t *testing.T) {
	_, err := Resolve(map[string]any{"LogLevel": "trace"}, nil, map[string]any{"Verbose": true}, map[string]any{})
	if !errors.Is(err, ErrInvalidLogLevel) {
		t.Fatalf("err = %v, want ErrInvalidLogLevel", err)
	}
}

func TestValidatorHook(t *testing.T) {
	errStub := errors.New("stub failure")
	orig := validators
	validators = append(slices.Clone(orig), func(Config) error { return errStub })
	t.Cleanup(func() { validators = orig })

	got, err := Resolve(nil, nil, nil, map[string]any{"LogLevel": "trace", "Verbose": true})
	if !errors.Is(err, ErrInvalidLogLevel) || !errors.Is(err, errStub) {
		t.Errorf("both failing: err = %v, want both sentinels", err)
	}
	if got != (Config{}) {
		t.Errorf("both failing: got %+v, want zero", got)
	}

	got, err = Resolve(nil, nil, nil, map[string]any{"LogLevel": "info", "Verbose": true})
	if errors.Is(err, ErrInvalidLogLevel) || !errors.Is(err, errStub) {
		t.Errorf("stub only: err = %v, want only stub sentinel", err)
	}
	if got != (Config{}) {
		t.Errorf("stub only: got %+v, want zero", got)
	}
}

type namedString string

func TestWrongTypePanics(t *testing.T) {
	cases := []struct {
		key  string
		val  any
		want []string
	}{
		{"MaxRetries", int64(3), []string{"int", "int64"}},
		{"MaxRetries", float64(3), []string{"int", "float64"}},
		{"MaxRetries", uint(3), []string{"int", "uint"}},
		{"MaxRetries", "3", []string{"int", "string"}},
		{"TimeoutSec", int64(3), []string{"int", "int64"}},
		{"TimeoutSec", float64(3), []string{"int", "float64"}},
		{"TimeoutSec", uint(3), []string{"int", "uint"}},
		{"TimeoutSec", "3", []string{"int", "string"}},
		{"Verbose", 1, []string{"bool", "int"}},
		{"Verbose", "true", []string{"bool", "string"}},
		{"LogLevel", []byte("info"), []string{"string", "[]uint8"}},
		{"LogLevel", namedString("info"), []string{"string", "namedString"}},
		{"Verbose", nil, []string{"bool", "nil"}},
		{"MaxRetries", nil, []string{"int", "nil"}},
		{"LogLevel", nil, []string{"string", "nil"}},
		{"TimeoutSec", nil, []string{"int", "nil"}},
		{"MaxRetries", (*int)(nil), []string{"int", "*int"}},
		{"TimeoutSec", (*int)(nil), []string{"int", "*int"}},
		{"LogLevel", (*string)(nil), []string{"string", "*string"}},
		{"Verbose", (*bool)(nil), []string{"bool", "*bool"}},
	}
	for _, tc := range cases {
		for pos := 0; pos < 4; pos++ {
			var ls [4]map[string]any
			ls[pos] = map[string]any{tc.key: tc.val}
			msg := mustPanic(t, func() { _, _ = resolveLayers(ls) })
			wantContains(t, msg, append([]string{layerNames[pos], tc.key}, tc.want...)...)
		}
	}
}

func TestWrongTypeInOverriddenLayerPanics(t *testing.T) {
	msg := mustPanic(t, func() {
		_, _ = Resolve(map[string]any{"MaxRetries": "3"}, nil, nil, map[string]any{"MaxRetries": 3})
	})
	wantContains(t, msg, "defaults", "MaxRetries")
}

func TestWrongTypeBeatsInvalidLevel(t *testing.T) {
	msg := mustPanic(t, func() {
		_, _ = Resolve(nil, map[string]any{"LogLevel": "trace", "TimeoutSec": 1.5}, nil, nil)
	})
	wantContains(t, msg, "file", "TimeoutSec")
}

func TestUnknownKeysPanic(t *testing.T) {
	for _, k := range []string{"Timeout", "loglevel", "LOGLEVEL", "verbose", ""} {
		msg := mustPanic(t, func() { _, _ = Resolve(nil, nil, map[string]any{k: 1}, nil) })
		wantContains(t, msg, "env", `"`+k+`"`)
	}
	msg := mustPanic(t, func() {
		_, _ = Resolve(nil, nil, nil, map[string]any{"zeta": 1, "Verbose": true, "alpha": 2, "mid": 3})
	})
	wantContains(t, msg, "flags", "alpha", "mid", "zeta")
	a, m, z := strings.Index(msg, "alpha"), strings.Index(msg, "mid"), strings.Index(msg, "zeta")
	if !(a < m && m < z) {
		t.Errorf("unknown keys not sorted in %q", msg)
	}
}

func TestInputsNotMutated(t *testing.T) {
	ls := [4]map[string]any{
		{"Verbose": true, "MaxRetries": 1},
		{"LogLevel": "info"},
		nil,
		{"TimeoutSec": 5, "MaxRetries": 0},
	}
	var before [4]map[string]any
	for i, m := range ls {
		if m != nil {
			before[i] = maps.Clone(m)
		}
	}
	if _, err := resolveLayers(ls); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ls, before) {
		t.Fatalf("inputs mutated: %v vs %v", ls, before)
	}
}

func TestFieldTableMatchesStruct(t *testing.T) {
	rt := reflect.TypeFor[Config]()
	byKey := map[string]field{}
	for _, f := range fields {
		byKey[f.key] = f
	}
	exported := 0
	for i := range rt.NumField() {
		sf := rt.Field(i)
		if !sf.IsExported() {
			continue
		}
		exported++
		f, ok := byKey[sf.Name]
		if !ok {
			t.Errorf("Config.%s has no fields entry", sf.Name)
			continue
		}
		if f.typ != sf.Type.String() {
			t.Errorf("fields[%q].typ = %s, struct type %s", sf.Name, f.typ, sf.Type)
		}
	}
	if exported != len(fields) {
		t.Errorf("Config has %d exported fields, table has %d entries", exported, len(fields))
	}
	for _, f := range fields {
		if _, ok := rt.FieldByName(f.key); !ok {
			t.Errorf("fields entry %q has no Config field", f.key)
		}
	}
}

func TestFieldKeysUnique(t *testing.T) {
	if len(fieldIndex) != len(fields) {
		t.Fatalf("len(fieldIndex)=%d, len(fields)=%d", len(fieldIndex), len(fields))
	}
	msg := mustPanic(t, func() {
		indexFields([]field{fields[0], fields[1], fields[0]})
	})
	wantContains(t, msg, fields[0].key)
}

func TestConcurrentResolve(t *testing.T) {
	var wg sync.WaitGroup
	shared := map[string]any{"Verbose": true, "LogLevel": "info"}
	for i := range 32 {
		wg.Go(func() {
			got, err := Resolve(shared, map[string]any{"MaxRetries": i}, nil, nil)
			if err != nil || got.MaxRetries != i || !got.Verbose {
				t.Errorf("goroutine %d: got %+v, %v", i, got, err)
			}
		})
	}
	wg.Wait()
}
