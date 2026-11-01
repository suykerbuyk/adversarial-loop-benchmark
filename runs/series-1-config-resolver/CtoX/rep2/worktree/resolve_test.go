package config

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"testing"
)

// resolveLayers calls Resolve with a fixed-size slice of layers.
func resolveLayers(ls [4]map[string]any) (Config, error) {
	return Resolve(ls[0], ls[1], ls[2], ls[3])
}

func TestPrecedenceMatrix(t *testing.T) {
	// For each field and every subset of presenting layers, the highest
	// presenter wins. The file and flags values are zero values, so zero-valued
	// presenters overriding non-zero lower values are covered.
	type fieldCase struct {
		key    string
		values [4]any
		get    func(Config) any
	}
	cases := []fieldCase{
		{"Verbose", [4]any{true, false, true, false}, func(c Config) any { return c.Verbose }},
		{"MaxRetries", [4]any{3, 0, 7, 0}, func(c Config) any { return c.MaxRetries }},
		{"LogLevel", [4]any{"info", "", "warn", ""}, func(c Config) any { return c.LogLevel }},
		{"TimeoutSec", [4]any{10, 0, 20, 0}, func(c Config) any { return c.TimeoutSec }},
	}
	zero := map[string]any{"Verbose": false, "MaxRetries": 0, "LogLevel": "", "TimeoutSec": 0}
	for _, fc := range cases {
		for mask := 0; mask < 16; mask++ {
			t.Run(fmt.Sprintf("%s/mask=%04b", fc.key, mask), func(t *testing.T) {
				var ls [4]map[string]any
				want := zero[fc.key]
				for i := range 4 {
					if mask&(1<<i) != 0 {
						ls[i] = map[string]any{fc.key: fc.values[i]}
						want = fc.values[i]
					}
				}
				got, err := resolveLayers(ls)
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if g := fc.get(got); g != want {
					t.Fatalf("%s = %v, want %v", fc.key, g, want)
				}
			})
		}
	}
}

func TestZeroPresenterOverridesNonZero(t *testing.T) {
	got, err := Resolve(
		map[string]any{"Verbose": true, "MaxRetries": 3, "LogLevel": "info", "TimeoutSec": 30},
		nil, nil,
		map[string]any{"Verbose": false, "MaxRetries": 0, "LogLevel": "", "TimeoutSec": 0})
	if err != nil {
		t.Fatal(err)
	}
	if got != (Config{}) {
		t.Fatalf("got %+v, want zero Config", got)
	}
}

func TestEmptyLayers(t *testing.T) {
	for name, ls := range map[string][4]map[string]any{
		"nil":   {},
		"empty": {{}, {}, {}, {}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := resolveLayers(ls)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != (Config{}) {
				t.Fatalf("got %+v, want zero Config", got)
			}
		})
	}
}

func TestMixedFieldsCombine(t *testing.T) {
	got, err := Resolve(
		map[string]any{"Verbose": false, "MaxRetries": 1, "LogLevel": "error", "TimeoutSec": 15},
		map[string]any{"LogLevel": "warn"},
		map[string]any{"MaxRetries": 9},
		map[string]any{"Verbose": true})
	if err != nil {
		t.Fatal(err)
	}
	want := Config{Verbose: true, MaxRetries: 9, LogLevel: "warn", TimeoutSec: 15}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestLegalLogLevels(t *testing.T) {
	for _, lvl := range []string{"debug", "info", "warn", "error", ""} {
		t.Run(lvl, func(t *testing.T) {
			got, err := Resolve(nil, nil, map[string]any{"LogLevel": lvl}, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.LogLevel != lvl {
				t.Fatalf("LogLevel = %q, want %q", got.LogLevel, lvl)
			}
		})
	}
}

func TestInvalidLogLevels(t *testing.T) {
	for _, lvl := range []string{"trace", "INFO", " info", "info ", "warning"} {
		t.Run(lvl, func(t *testing.T) {
			got, err := Resolve(map[string]any{"Verbose": true}, nil, nil, map[string]any{"LogLevel": lvl})
			if !errors.Is(err, ErrInvalidLogLevel) {
				t.Fatalf("err = %v, want ErrInvalidLogLevel", err)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("%q", lvl)) {
				t.Errorf("error %q does not mention the bad level", err)
			}
			if got != (Config{}) {
				t.Fatalf("got %+v, want zero Config on error", got)
			}
		})
	}
}

func TestOverriddenInvalidLevelNotValidated(t *testing.T) {
	for _, top := range []string{"info", ""} {
		t.Run(fmt.Sprintf("top=%q", top), func(t *testing.T) {
			got, err := Resolve(
				map[string]any{"LogLevel": "trace"},
				map[string]any{"LogLevel": "bogus"},
				nil,
				map[string]any{"LogLevel": top})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.LogLevel != top {
				t.Fatalf("LogLevel = %q, want %q", got.LogLevel, top)
			}
		})
	}
}

func TestInvalidLowerLevelShowsThrough(t *testing.T) {
	_, err := Resolve(
		map[string]any{"LogLevel": "info"},
		map[string]any{"LogLevel": "bogus"},
		nil,
		map[string]any{"Verbose": true})
	if !errors.Is(err, ErrInvalidLogLevel) {
		t.Fatalf("err = %v, want ErrInvalidLogLevel", err)
	}
}

type myBool bool

func TestProgrammerErrorsPanic(t *testing.T) {
	cases := []struct {
		name   string
		layers [4]map[string]any
		layer  string
		key    string
	}{
		{"Verbose string", [4]map[string]any{{"Verbose": "true"}}, "defaults", "Verbose"},
		{"MaxRetries int64", [4]map[string]any{nil, {"MaxRetries": int64(1)}}, "file", "MaxRetries"},
		{"MaxRetries float", [4]map[string]any{nil, nil, {"MaxRetries": 1.0}}, "env", "MaxRetries"},
		{"LogLevel int", [4]map[string]any{nil, nil, nil, {"LogLevel": 1}}, "flags", "LogLevel"},
		{"TimeoutSec string", [4]map[string]any{{"TimeoutSec": "30"}}, "defaults", "TimeoutSec"},
		{"nil value", [4]map[string]any{nil, {"LogLevel": nil}}, "file", "LogLevel"},
		{"named type", [4]map[string]any{nil, nil, {"Verbose": myBool(true)}}, "env", "Verbose"},
		{"overridden wrong type", [4]map[string]any{{"MaxRetries": "3"}, nil, nil, {"MaxRetries": 3}}, "defaults", "MaxRetries"},
		{"unknown key", [4]map[string]any{nil, nil, nil, {"Loglevel": "info"}}, "flags", "Loglevel"},
		{"wrong type with invalid level", [4]map[string]any{{"LogLevel": "trace", "Verbose": 1}}, "defaults", "Verbose"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := mustPanic(t, func() { _, _ = resolveLayers(tc.layers) })
			if !strings.Contains(msg, fmt.Sprintf("%q", tc.layer)) || !strings.Contains(msg, fmt.Sprintf("%q", tc.key)) {
				t.Errorf("panic message %q should name layer %q and key %q", msg, tc.layer, tc.key)
			}
		})
	}
}

// mustPanic runs f, fails the test if it does not panic, and returns the
// panic value formatted as a string.
func mustPanic(t *testing.T, f func()) (msg string) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic, got none")
		}
		msg = fmt.Sprint(r)
	}()
	f()
	return ""
}

func TestInputsNotMutated(t *testing.T) {
	ls := [4]map[string]any{
		{"Verbose": true, "MaxRetries": 3, "LogLevel": "info", "TimeoutSec": 30},
		{"LogLevel": ""},
		{"MaxRetries": 0},
		{"Verbose": false},
	}
	var before [4]map[string]any
	for i, m := range ls {
		before[i] = maps.Clone(m)
	}
	if _, err := resolveLayers(ls); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, ls) {
		t.Fatalf("inputs mutated: before %v, after %v", before, ls)
	}
}

// TestFieldTableInSync guards that every Config field has exactly one table
// entry with a matching type, and that each entry stores into its own field.
func TestFieldTableInSync(t *testing.T) {
	typ := reflect.TypeFor[Config]()
	if len(fields) != typ.NumField() {
		t.Fatalf("field table has %d entries, Config has %d fields", len(fields), typ.NumField())
	}
	seen := map[string]bool{}
	for _, f := range fields {
		if seen[f.key] {
			t.Fatalf("duplicate table key %q", f.key)
		}
		seen[f.key] = true
		sf, ok := typ.FieldByName(f.key)
		if !ok {
			t.Fatalf("table key %q has no Config field", f.key)
		}
		// A non-zero value of the struct field's type must be accepted and
		// must land in that field and no other.
		v := nonZero(t, sf.Type)
		var c Config
		f.apply(&c, "test", v.Interface())
		var want Config
		reflect.ValueOf(&want).Elem().FieldByIndex(sf.Index).Set(v)
		if c != want {
			t.Fatalf("applying %q = %v gave %+v, want %+v", f.key, v, c, want)
		}
	}
	if len(fieldByKey) != len(fields) {
		t.Fatalf("fieldByKey has %d entries, want %d", len(fieldByKey), len(fields))
	}
}

func nonZero(t *testing.T, typ reflect.Type) reflect.Value {
	t.Helper()
	v := reflect.New(typ).Elem()
	switch typ.Kind() {
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int:
		v.SetInt(1)
	case reflect.String:
		v.SetString("x")
	default:
		t.Fatalf("nonZero: unsupported kind %s; extend the test", typ.Kind())
	}
	return v
}

func ExampleResolve() {
	cfg, err := Resolve(
		map[string]any{"Verbose": true, "MaxRetries": 3, "LogLevel": "info", "TimeoutSec": 30},
		map[string]any{"LogLevel": "warn"},
		map[string]any{"MaxRetries": 0},
		map[string]any{"Verbose": false},
	)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("%+v\n", cfg)
	// Output: {Verbose:false MaxRetries:0 LogLevel:warn TimeoutSec:30}
}
