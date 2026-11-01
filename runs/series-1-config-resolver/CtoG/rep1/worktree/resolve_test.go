package config

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"testing"
)

var layerNames = []string{"defaults", "file", "env", "flags"}

// resolveAt builds four layers, placing each non-nil map at its index.
func resolveAt(ls [4]map[string]any) (Config, error) {
	return Resolve(ls[0], ls[1], ls[2], ls[3])
}

func mustPanic(t *testing.T, f func()) (msg string) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected panic, got none")
		}
		msg = fmt.Sprint(r)
	}()
	f()
	return ""
}

func TestZeroValueOverridesEveryLayer(t *testing.T) {
	cases := []struct {
		key     string
		nonZero any
		zero    any
		get     func(Config) any
	}{
		{"Verbose", true, false, func(c Config) any { return c.Verbose }},
		{"MaxRetries", 7, 0, func(c Config) any { return c.MaxRetries }},
		{"TimeoutSec", 9, 0, func(c Config) any { return c.TimeoutSec }},
		{"LogLevel", "info", "", func(c Config) any { return c.LogLevel }},
	}
	for _, tc := range cases {
		for i := 1; i < 4; i++ {
			t.Run(tc.key+"/"+layerNames[i], func(t *testing.T) {
				var ls [4]map[string]any
				ls[0] = map[string]any{tc.key: tc.nonZero}
				ls[i] = map[string]any{tc.key: tc.zero}
				got, err := resolveAt(ls)
				if err != nil {
					t.Fatal(err)
				}
				if g := tc.get(got); g != tc.zero {
					t.Fatalf("%s = %#v, want %#v", tc.key, g, tc.zero)
				}
			})
		}
	}
}

func TestPrecedenceEveryPair(t *testing.T) {
	for i := 0; i < 4; i++ {
		for j := i + 1; j < 4; j++ {
			t.Run(layerNames[i]+"<"+layerNames[j], func(t *testing.T) {
				var ls [4]map[string]any
				ls[i] = map[string]any{"Verbose": false, "MaxRetries": 1, "LogLevel": "debug", "TimeoutSec": 1}
				ls[j] = map[string]any{"Verbose": true, "MaxRetries": 2, "LogLevel": "error", "TimeoutSec": 2}
				got, err := resolveAt(ls)
				if err != nil {
					t.Fatal(err)
				}
				want := Config{Verbose: true, MaxRetries: 2, LogLevel: "error", TimeoutSec: 2}
				if got != want {
					t.Fatalf("got %+v, want %+v", got, want)
				}
			})
		}
	}
}

func TestAbsentPassThroughAcrossGaps(t *testing.T) {
	got, err := Resolve(
		map[string]any{"MaxRetries": 4},
		map[string]any{"Verbose": true},
		map[string]any{"TimeoutSec": 5},
		map[string]any{"LogLevel": "warn"})
	if err != nil {
		t.Fatal(err)
	}
	want := Config{Verbose: true, MaxRetries: 4, LogLevel: "warn", TimeoutSec: 5}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	got, err = Resolve(nil, map[string]any{"TimeoutSec": 8}, map[string]any{}, map[string]any{"Verbose": false})
	if err != nil {
		t.Fatal(err)
	}
	if got.TimeoutSec != 8 {
		t.Fatalf("TimeoutSec = %d, want 8", got.TimeoutSec)
	}
}

func TestNothingPresented(t *testing.T) {
	for name, ls := range map[string][4]map[string]any{
		"nil":   {},
		"empty": {{}, {}, {}, {}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := resolveAt(ls)
			if err != nil {
				t.Fatal(err)
			}
			if got != (Config{}) {
				t.Fatalf("got %+v, want zero Config", got)
			}
		})
	}
}

func TestOnlyEffectiveLogLevelValidated(t *testing.T) {
	got, err := Resolve(map[string]any{"LogLevel": "trace"}, nil, nil, map[string]any{"LogLevel": "info"})
	if err != nil {
		t.Fatalf("overridden invalid level should not be validated: %v", err)
	}
	if got.LogLevel != "info" {
		t.Fatalf("LogLevel = %q, want info", got.LogLevel)
	}

	got, err = Resolve(map[string]any{"LogLevel": "info", "MaxRetries": 3}, nil, nil, map[string]any{"LogLevel": "trace"})
	if !errors.Is(err, ErrInvalidLogLevel) {
		t.Fatalf("err = %v, want ErrInvalidLogLevel", err)
	}
	if got != (Config{}) {
		t.Fatalf("on error got %+v, want zero Config", got)
	}
	if msg := err.Error(); !strings.Contains(msg, `"trace"`) || !strings.Contains(msg, `"flags"`) {
		t.Fatalf("error message %q should name value and layer", msg)
	}
}

func TestLegalLogLevels(t *testing.T) {
	for _, lvl := range []string{"", "debug", "info", "warn", "error"} {
		got, err := Resolve(nil, nil, map[string]any{"LogLevel": lvl}, nil)
		if err != nil {
			t.Errorf("level %q: unexpected error %v", lvl, err)
		}
		if got.LogLevel != lvl {
			t.Errorf("LogLevel = %q, want %q", got.LogLevel, lvl)
		}
	}
}

func TestIllegalLogLevels(t *testing.T) {
	for _, lvl := range []string{"trace", "INFO", "Debug", " info", "warning"} {
		_, err := Resolve(map[string]any{"LogLevel": lvl}, nil, nil, nil)
		if !errors.Is(err, ErrInvalidLogLevel) {
			t.Errorf("level %q: err = %v, want ErrInvalidLogLevel", lvl, err)
		}
	}
}

func TestOtherFieldsNotValidated(t *testing.T) {
	got, err := Resolve(map[string]any{"MaxRetries": -1, "TimeoutSec": -5}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxRetries != -1 || got.TimeoutSec != -5 {
		t.Fatalf("got %+v", got)
	}
}

type lvl string

func TestWrongTypePanics(t *testing.T) {
	cases := map[string]map[string]any{
		"int64":      {"MaxRetries": int64(3)},
		"float":      {"MaxRetries": 3.0},
		"string int": {"MaxRetries": "3"},
		"int bool":   {"Verbose": 1},
		"str bool":   {"Verbose": "true"},
		"bytes":      {"LogLevel": []byte("info")},
		"nil":        {"LogLevel": nil},
		"named":      {"LogLevel": lvl("info")},
	}
	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			mustPanic(t, func() { _, _ = Resolve(nil, m, nil, nil) })
		})
	}
	t.Run("overridden", func(t *testing.T) {
		msg := mustPanic(t, func() {
			_, _ = Resolve(map[string]any{"TimeoutSec": "10"}, nil, nil, map[string]any{"TimeoutSec": 10})
		})
		if !strings.Contains(msg, "TimeoutSec") || !strings.Contains(msg, "defaults") {
			t.Fatalf("panic message %q should name key and layer", msg)
		}
	})
}

func TestUnknownKeyPanics(t *testing.T) {
	for _, key := range []string{"verbose", "Timeout"} {
		t.Run(key, func(t *testing.T) {
			msg := mustPanic(t, func() { _, _ = Resolve(nil, nil, map[string]any{key: true}, nil) })
			if !strings.Contains(msg, key) || !strings.Contains(msg, "env") {
				t.Fatalf("panic message %q should name key and layer", msg)
			}
		})
	}
}

func TestInputsNotMutated(t *testing.T) {
	ls := [4]map[string]any{
		{"Verbose": true, "MaxRetries": 1},
		{"LogLevel": "debug"},
		{"TimeoutSec": 3},
		{"Verbose": false},
	}
	var snap [4]map[string]any
	for i := range ls {
		snap[i] = maps.Clone(ls[i])
	}
	if _, err := resolveAt(ls); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ls, snap) {
		t.Fatalf("inputs mutated: %v vs %v", ls, snap)
	}
}

// TestFieldTableMatchesStruct guards against Config and the field table
// drifting apart as fields are added.
func TestFieldTableMatchesStruct(t *testing.T) {
	typ := reflect.TypeFor[Config]()
	if typ.NumField() != len(fields) {
		t.Fatalf("Config has %d fields, table has %d", typ.NumField(), len(fields))
	}
	for i := 0; i < typ.NumField(); i++ {
		sf := typ.Field(i)
		f, ok := fieldsByKey[sf.Name]
		if !ok {
			t.Errorf("Config.%s has no field table entry", sf.Name)
			continue
		}
		// Setting a zero value of the struct field's type must not panic and
		// must land in that struct field.
		var c Config
		v := reflect.New(sf.Type).Elem()
		switch sf.Type.Kind() {
		case reflect.Bool:
			v.SetBool(true)
		case reflect.Int:
			v.SetInt(42)
		case reflect.String:
			v.SetString("x")
		default:
			t.Fatalf("unhandled kind %v; extend this test", sf.Type.Kind())
		}
		f.set(&c, "test", v.Interface())
		if got := reflect.ValueOf(c).Field(i).Interface(); got != v.Interface() {
			t.Errorf("key %q set Config.%s to %#v, want %#v", f.key, sf.Name, got, v.Interface())
		}
	}
}

func ExampleResolve() {
	cfg, err := Resolve(
		map[string]any{"Verbose": true, "MaxRetries": 3, "LogLevel": "info", "TimeoutSec": 30},
		map[string]any{"LogLevel": "debug"},
		map[string]any{"MaxRetries": 0},
		map[string]any{"Verbose": false},
	)
	fmt.Printf("%+v %v\n", cfg, err)
	// Output: {Verbose:false MaxRetries:0 LogLevel:debug TimeoutSec:30} <nil>
}
