package config

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

var layerNames = []string{"defaults", "file", "env", "flags"}

// resolveAt builds four layers with m at position pos (others nil) and resolves.
func resolveAt(pos int, m map[string]any) (Config, error) {
	ls := make([]map[string]any, 4)
	ls[pos] = m
	return Resolve(ls[0], ls[1], ls[2], ls[3])
}

// mustPanic runs f and returns the panic message; it fails the test if f does not panic.
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

func TestT1AllNil(t *testing.T) {
	got, err := Resolve(nil, nil, nil, nil)
	if err != nil || got != (Config{}) {
		t.Fatalf("got %+v, %v; want zero, nil", got, err)
	}
}

func TestT2AllEmpty(t *testing.T) {
	e := func() map[string]any { return map[string]any{} }
	got, err := Resolve(e(), e(), e(), e())
	if err != nil || got != (Config{}) {
		t.Fatalf("got %+v, %v; want zero, nil", got, err)
	}
}

func TestT3OverriddenInvalidNotValidated(t *testing.T) {
	got, err := Resolve(map[string]any{"LogLevel": "trace"}, nil, nil, map[string]any{"LogLevel": "info"})
	if err != nil || got.LogLevel != "info" {
		t.Fatalf("got %+v, %v", got, err)
	}
	got, err = Resolve(nil, map[string]any{"LogLevel": "bogus"}, map[string]any{"LogLevel": ""}, nil)
	if err != nil || got.LogLevel != "" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestT4LegalLevelsEveryLayer(t *testing.T) {
	for _, lvl := range []string{"debug", "info", "warn", "error", ""} {
		for pos, name := range layerNames {
			got, err := resolveAt(pos, map[string]any{"LogLevel": lvl})
			if err != nil || got.LogLevel != lvl {
				t.Errorf("%s=%q: got %+v, %v", name, lvl, got, err)
			}
		}
	}
}

func TestT5InvalidLevels(t *testing.T) {
	for _, lvl := range []string{"trace", "INFO", " info", "Debug"} {
		got, err := Resolve(map[string]any{"Verbose": true, "LogLevel": lvl}, nil, nil, nil)
		if !errors.Is(err, ErrInvalidLogLevel) {
			t.Errorf("%q: err = %v, want ErrInvalidLogLevel", lvl, err)
			continue
		}
		if got != (Config{}) {
			t.Errorf("%q: config = %+v, want zero", lvl, got)
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("%q", lvl)) {
			t.Errorf("%q: message %q lacks bad value", lvl, err)
		}
	}
}

func TestT6ZeroOverridesEveryPair(t *testing.T) {
	cases := []struct {
		key       string
		low, high any
		check     func(Config) bool
	}{
		{"Verbose", true, false, func(c Config) bool { return !c.Verbose }},
		{"MaxRetries", 3, 0, func(c Config) bool { return c.MaxRetries == 0 }},
		{"LogLevel", "info", "", func(c Config) bool { return c.LogLevel == "" }},
		{"TimeoutSec", 30, 0, func(c Config) bool { return c.TimeoutSec == 0 }},
	}
	for _, tc := range cases {
		for lo := 0; lo < 4; lo++ {
			for hi := lo + 1; hi < 4; hi++ {
				ls := make([]map[string]any, 4)
				ls[lo] = map[string]any{tc.key: tc.low}
				ls[hi] = map[string]any{tc.key: tc.high}
				got, err := Resolve(ls[0], ls[1], ls[2], ls[3])
				if err != nil || !tc.check(got) {
					t.Errorf("%s %s->%s: got %+v, %v", tc.key, layerNames[lo], layerNames[hi], got, err)
				}
			}
		}
	}
}

func TestT7PassThroughMiddle(t *testing.T) {
	got, err := Resolve(
		map[string]any{"MaxRetries": 1},
		map[string]any{"MaxRetries": 2},
		map[string]any{"Verbose": true},
		map[string]any{"TimeoutSec": 9})
	want := Config{Verbose: true, MaxRetries: 2, TimeoutSec: 9}
	if err != nil || got != want {
		t.Fatalf("got %+v, %v; want %+v", got, err, want)
	}
}

func TestT8MixedWinners(t *testing.T) {
	got, err := Resolve(
		map[string]any{"Verbose": true, "MaxRetries": 1, "LogLevel": "debug", "TimeoutSec": 1},
		map[string]any{"MaxRetries": 2, "LogLevel": "info", "TimeoutSec": 2},
		map[string]any{"LogLevel": "warn", "TimeoutSec": 3},
		map[string]any{"TimeoutSec": 4})
	want := Config{Verbose: true, MaxRetries: 2, LogLevel: "warn", TimeoutSec: 4}
	if err != nil || got != want {
		t.Fatalf("got %+v, %v; want %+v", got, err, want)
	}
}

type myInt int

func TestT9WrongTypePanics(t *testing.T) {
	cases := []struct {
		key string
		val any
	}{
		{"Verbose", "true"},
		{"Verbose", 1},
		{"MaxRetries", int64(1)},
		{"MaxRetries", 1.0},
		{"LogLevel", []byte("x")},
		{"TimeoutSec", "30"},
		{"TimeoutSec", myInt(30)},
		{"LogLevel", nil},
	}
	for _, tc := range cases {
		for pos, name := range layerNames {
			msg := mustPanic(t, func() { resolveAt(pos, map[string]any{tc.key: tc.val}) })
			if !strings.Contains(msg, tc.key) || !strings.Contains(msg, name) {
				t.Errorf("%s %s=%#v: panic %q lacks key/layer", name, tc.key, tc.val, msg)
			}
		}
	}
}

func TestT10WrongTypeInOverriddenLayerPanics(t *testing.T) {
	mustPanic(t, func() {
		Resolve(map[string]any{"MaxRetries": "3"}, nil, nil, map[string]any{"MaxRetries": 4})
	})
}

func TestT11UnknownKeyPanics(t *testing.T) {
	for _, key := range []string{"Loglevel", "timeout"} {
		for pos, name := range layerNames {
			for _, m := range []map[string]any{
				{key: 1},
				{key: 1, "Verbose": true, "LogLevel": "info"},
			} {
				msg := mustPanic(t, func() { resolveAt(pos, m) })
				if !strings.Contains(msg, fmt.Sprintf("%q", key)) || !strings.Contains(msg, name) {
					t.Errorf("%s %v: panic %q lacks key/layer", name, m, msg)
				}
			}
		}
	}
	for i := 0; i < 50; i++ {
		msg := mustPanic(t, func() {
			Resolve(map[string]any{"zeta": 1, "alpha": 2, "Verbose": true, "mid": 3}, nil, nil, nil)
		})
		if !strings.Contains(msg, `"alpha"`) {
			t.Fatalf("panic %q does not name smallest unknown key", msg)
		}
	}
}

func TestT12InputsNotMutated(t *testing.T) {
	mk := func() []map[string]any {
		return []map[string]any{
			{"Verbose": true, "MaxRetries": 3, "LogLevel": "info", "TimeoutSec": 10},
			{"LogLevel": ""},
			{"MaxRetries": 0},
			{"Verbose": false},
		}
	}
	in, want := mk(), mk()
	if _, err := Resolve(in[0], in[1], in[2], in[3]); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, want) {
		t.Fatalf("inputs mutated: %v, want %v", in, want)
	}
}

func TestT13FieldTableSync(t *testing.T) {
	ct := reflect.TypeOf(Config{})
	table := fieldTable()
	if len(table) != ct.NumField() {
		t.Fatalf("table has %d rows, Config has %d fields", len(table), ct.NumField())
	}
	seen := map[string]bool{}
	for _, f := range table {
		if f.key == "" || f.set == nil {
			t.Fatalf("bad row %+v", f)
		}
		if seen[f.key] {
			t.Fatalf("duplicate key %q", f.key)
		}
		seen[f.key] = true
		if sf, ok := ct.FieldByName(f.key); !ok || !sf.IsExported() {
			t.Fatalf("key %q is not an exported Config field", f.key)
		}
	}
	for i := 0; i < ct.NumField(); i++ {
		if name := ct.Field(i).Name; !seen[name] {
			t.Fatalf("Config field %q has no table row", name)
		}
	}

	base := Config{Verbose: true, MaxRetries: 101, LogLevel: "base", TimeoutSec: 202}
	for _, f := range table {
		sf, _ := ct.FieldByName(f.key)
		var probe any
		switch sf.Type.Kind() {
		case reflect.Bool:
			probe = false
		case reflect.Int:
			probe = 987
		case reflect.String:
			probe = "probe"
		default:
			t.Fatalf("field %q has kind %s; extend this guard", f.key, sf.Type.Kind())
		}
		c := base
		if !f.set(&c, probe) {
			t.Fatalf("%q: set rejected probe %#v", f.key, probe)
		}
		cv, bv := reflect.ValueOf(c), reflect.ValueOf(base)
		for i := 0; i < ct.NumField(); i++ {
			name := ct.Field(i).Name
			got := cv.Field(i).Interface()
			if name == f.key {
				if got != probe {
					t.Errorf("%q: field = %#v, want %#v", f.key, got, probe)
				}
			} else if got != bv.Field(i).Interface() {
				t.Errorf("%q: set changed unrelated field %q", f.key, name)
			}
		}

		c = base
		if f.set(&c, struct{}{}) {
			t.Errorf("%q: set accepted struct{}{}", f.key)
		}
		if c != base {
			t.Errorf("%q: rejected set modified config: %+v", f.key, c)
		}
	}

	first := fieldTable()
	first[0].key = "x"
	if second := fieldTable(); second[0].key != table[0].key {
		t.Fatalf("fieldTable shares state: got %q", second[0].key)
	}
}

func TestT14Concurrent(t *testing.T) {
	d := map[string]any{"Verbose": true, "MaxRetries": 3, "LogLevel": "info", "TimeoutSec": 10}
	f := map[string]any{"LogLevel": "debug"}
	e := map[string]any{"MaxRetries": 0}
	fl := map[string]any{"Verbose": false}
	want := Config{MaxRetries: 0, LogLevel: "debug", TimeoutSec: 10}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				got, err := Resolve(d, f, e, fl)
				if err != nil || got != want {
					t.Errorf("got %+v, %v", got, err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestT15SetterWiringEndToEnd(t *testing.T) {
	cases := []struct {
		key  string
		val  any
		want Config
	}{
		{"Verbose", true, Config{Verbose: true}},
		{"MaxRetries", 7, Config{MaxRetries: 7}},
		{"LogLevel", "warn", Config{LogLevel: "warn"}},
		{"TimeoutSec", 11, Config{TimeoutSec: 11}},
	}
	for _, tc := range cases {
		for pos, name := range layerNames {
			got, err := resolveAt(pos, map[string]any{tc.key: tc.val})
			if err != nil || got != tc.want {
				t.Errorf("%s %s: got %+v, %v; want %+v", name, tc.key, got, err, tc.want)
			}
		}
	}
}
