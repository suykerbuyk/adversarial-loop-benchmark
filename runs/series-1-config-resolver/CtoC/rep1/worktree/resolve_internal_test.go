package config

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"sync"
	"testing"
)

var layerNames = []string{"defaults", "file", "env", "flags"}

// resolveN calls Resolve with exactly four layers taken from ls.
func resolveN(ls [4]map[string]any) (Config, error) {
	return Resolve(ls[0], ls[1], ls[2], ls[3])
}

func mustPanic(t *testing.T, wantSubstr string, fn func()) {
	t.Helper()
	defer func() {
		t.Helper()
		r := recover()
		if r == nil {
			t.Fatalf("expected panic, got none")
		}
		msg := fmt.Sprint(r)
		if wantSubstr != "" && !strings.Contains(msg, wantSubstr) {
			t.Fatalf("panic message %q does not contain %q", msg, wantSubstr)
		}
	}()
	fn()
}

func TestEmptyLayers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		layers [4]map[string]any
	}{
		{"nil", [4]map[string]any{}},
		{"empty", [4]map[string]any{{}, {}, {}, {}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveN(tc.layers)
			if err != nil || got != (Config{}) {
				t.Fatalf("got (%+v, %v), want zero Config and nil", got, err)
			}
		})
	}
}

// fieldCase gives two distinct values (one zero, one non-zero) per field.
type fieldCase struct {
	key       string
	zero, set any
	get       func(Config) any
}

var fieldCases = []fieldCase{
	{"Verbose", false, true, func(c Config) any { return c.Verbose }},
	{"MaxRetries", 0, 7, func(c Config) any { return c.MaxRetries }},
	{"LogLevel", "", "warn", func(c Config) any { return c.LogLevel }},
	{"TimeoutSec", 0, 42, func(c Config) any { return c.TimeoutSec }},
}

func TestOverrideMatrix(t *testing.T) {
	for _, fc := range fieldCases {
		// Every non-empty subset of layers presents the field; values
		// alternate so adjacent layers differ. Highest presenting wins.
		for mask := 1; mask < 16; mask++ {
			var ls [4]map[string]any
			var want any
			for i := 0; i < 4; i++ {
				if mask&(1<<i) == 0 {
					continue
				}
				v := fc.set
				if i%2 == 1 {
					v = fc.zero
				}
				ls[i] = map[string]any{fc.key: v}
				want = v
			}
			got, err := resolveN(ls)
			if err != nil {
				t.Fatalf("%s mask %04b: %v", fc.key, mask, err)
			}
			if g := fc.get(got); g != want {
				t.Errorf("%s mask %04b: got %v, want %v", fc.key, mask, g, want)
			}
		}
	}
}

func TestZeroOverridesInEveryPosition(t *testing.T) {
	for _, fc := range fieldCases {
		for hi := 1; hi < 4; hi++ {
			for lo := 0; lo < hi; lo++ {
				var ls [4]map[string]any
				ls[lo] = map[string]any{fc.key: fc.set}
				ls[hi] = map[string]any{fc.key: fc.zero}
				got, err := resolveN(ls)
				if err != nil {
					t.Fatal(err)
				}
				if g := fc.get(got); g != fc.zero {
					t.Errorf("%s: zero in %s over set in %s: got %v", fc.key, layerNames[hi], layerNames[lo], g)
				}
			}
		}
	}
}

func TestZeroSurvivesAbsentHigherLayers(t *testing.T) {
	for _, fc := range fieldCases {
		ls := [4]map[string]any{
			{fc.key: fc.set},
			{fc.key: fc.zero},
			{},
			nil,
		}
		got, err := resolveN(ls)
		if err != nil {
			t.Fatal(err)
		}
		if g := fc.get(got); g != fc.zero {
			t.Errorf("%s: got %v, want zero %v", fc.key, g, fc.zero)
		}
	}
}

func TestValidLogLevels(t *testing.T) {
	for _, lvl := range []string{"debug", "info", "warn", "error", ""} {
		got, err := Resolve(nil, nil, nil, map[string]any{"LogLevel": lvl})
		if err != nil {
			t.Errorf("level %q: unexpected error %v", lvl, err)
		}
		if got.LogLevel != lvl {
			t.Errorf("level %q: got %q", lvl, got.LogLevel)
		}
	}
}

func TestInvalidLogLevels(t *testing.T) {
	for _, lvl := range []string{"trace", "INFO", "Info", " info", "warning"} {
		got, err := Resolve(map[string]any{"Verbose": true}, nil, nil, map[string]any{"LogLevel": lvl})
		if !errors.Is(err, ErrInvalidLogLevel) {
			t.Errorf("level %q: err = %v, want ErrInvalidLogLevel", lvl, err)
			continue
		}
		if got != (Config{}) {
			t.Errorf("level %q: got %+v, want zero Config on error", lvl, got)
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("%q", lvl)) {
			t.Errorf("level %q: message %q does not name the value", lvl, err)
		}
	}
}

func TestOverriddenInvalidLevelNotValidated(t *testing.T) {
	for hi := 1; hi < 4; hi++ {
		for lo := 0; lo < hi; lo++ {
			for _, good := range []string{"info", ""} {
				var ls [4]map[string]any
				ls[lo] = map[string]any{"LogLevel": "trace"}
				ls[hi] = map[string]any{"LogLevel": good}
				got, err := resolveN(ls)
				if err != nil {
					t.Errorf("trace in %s, %q in %s: %v", layerNames[lo], good, layerNames[hi], err)
				}
				if got.LogLevel != good {
					t.Errorf("got %q, want %q", got.LogLevel, good)
				}
			}
		}
	}
}

func TestValidOverriddenByInvalid(t *testing.T) {
	_, err := Resolve(map[string]any{"LogLevel": "info"}, nil, map[string]any{"LogLevel": "bogus"}, nil)
	if !errors.Is(err, ErrInvalidLogLevel) {
		t.Fatalf("err = %v, want ErrInvalidLogLevel", err)
	}
}

type namedLevel string

func TestWrongTypePanics(t *testing.T) {
	cases := []struct {
		key string
		val any
	}{
		{"MaxRetries", int64(1)},
		{"MaxRetries", 1.0},
		{"MaxRetries", "1"},
		{"MaxRetries", uint(1)},
		{"TimeoutSec", int32(1)},
		{"TimeoutSec", 1.5},
		{"Verbose", 1},
		{"Verbose", "true"},
		{"LogLevel", 1},
		{"LogLevel", namedLevel("info")},
		{"LogLevel", nil},
		{"Verbose", nil},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s=%T", tc.key, tc.val), func(t *testing.T) {
			mustPanic(t, tc.key, func() {
				_, _ = Resolve(nil, map[string]any{tc.key: tc.val}, nil, nil)
			})
		})
	}
}

func TestWrongTypeInOverriddenLayerPanics(t *testing.T) {
	mustPanic(t, "MaxRetries", func() {
		_, _ = Resolve(map[string]any{"MaxRetries": "3"}, nil, nil, map[string]any{"MaxRetries": 3})
	})
}

func TestUnknownKeyPanics(t *testing.T) {
	for _, key := range []string{"loglevel", "Loglevel", "logLevel", "Timeout", ""} {
		t.Run(key, func(t *testing.T) {
			mustPanic(t, fmt.Sprintf("%q", key), func() {
				_, _ = Resolve(nil, nil, map[string]any{"Verbose": true, key: "x"}, nil)
			})
		})
	}
}

func TestUnknownKeysSortedInMessage(t *testing.T) {
	mustPanic(t, `["a" "b" "c"]`, func() {
		_, _ = Resolve(map[string]any{"c": 1, "a": 1, "b": 1, "LogLevel": "info"}, nil, nil, nil)
	})
}

func TestInputsNotMutated(t *testing.T) {
	ls := [4]map[string]any{
		{"Verbose": true, "MaxRetries": 3, "LogLevel": "info", "TimeoutSec": 10},
		{"LogLevel": ""},
		{"MaxRetries": 0},
		{"Verbose": false},
	}
	var before [4]map[string]any
	for i, l := range ls {
		before[i] = maps.Clone(l)
	}
	if _, err := resolveN(ls); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ls, before) {
		t.Fatalf("inputs mutated: got %v, want %v", ls, before)
	}
}

// TestFieldsTableInSync guards that every Config field has exactly one
// fields entry, bound to the correct Go type.
func TestFieldsTableInSync(t *testing.T) {
	typ := reflect.TypeFor[Config]()
	seen := map[string]int{}
	for _, f := range fields {
		seen[f.name]++
	}
	if len(fields) != typ.NumField() {
		t.Errorf("fields has %d entries, Config has %d fields", len(fields), typ.NumField())
	}
	for i := 0; i < typ.NumField(); i++ {
		sf := typ.Field(i)
		if seen[sf.Name] != 1 {
			t.Errorf("Config.%s: %d fields entries, want 1", sf.Name, seen[sf.Name])
			continue
		}
		// Applying a zero value of the field's type must not panic and
		// must leave the field at that value; a non-matching type must panic.
		for _, f := range fields {
			if f.name != sf.Name {
				continue
			}
			var c Config
			f.apply(&c, reflect.Zero(sf.Type).Interface())
			mustPanic(t, sf.Name, func() { f.apply(&c, struct{}{}) })
		}
	}
}

func TestConcurrentResolve(t *testing.T) {
	ls := [4]map[string]any{
		{"Verbose": true, "MaxRetries": 3},
		{"LogLevel": "debug"},
		nil,
		{"TimeoutSec": 9},
	}
	want := Config{Verbose: true, MaxRetries: 3, LogLevel: "debug", TimeoutSec: 9}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 100 {
				got, err := resolveN(ls)
				if err != nil || got != want {
					t.Errorf("got (%+v, %v), want %+v", got, err, want)
					return
				}
			}
		})
	}
	wg.Wait()
}
