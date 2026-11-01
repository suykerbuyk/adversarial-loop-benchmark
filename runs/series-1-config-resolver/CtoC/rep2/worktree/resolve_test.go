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

type m = map[string]any

func TestOverrideSemantics(t *testing.T) {
	full := m{"Verbose": true, "MaxRetries": 3, "LogLevel": "info", "TimeoutSec": 30}
	zeros := m{"Verbose": false, "MaxRetries": 0, "LogLevel": "", "TimeoutSec": 0}
	tests := []struct {
		name                       string
		defaults, file, env, flags m
		want                       Config
	}{
		{name: "all nil", want: Config{}},
		{name: "all empty", defaults: m{}, file: m{}, env: m{}, flags: m{}, want: Config{}},
		{name: "file presents zeros", defaults: full, file: zeros, want: Config{}},
		{name: "env presents zeros", defaults: full, env: zeros, want: Config{}},
		{name: "flags presents zeros", defaults: full, flags: zeros, want: Config{}},
		{
			name:     "defaults show through all gaps",
			defaults: full, file: m{}, env: nil, flags: m{},
			want: Config{Verbose: true, MaxRetries: 3, LogLevel: "info", TimeoutSec: 30},
		},
		{
			name:     "middle layer shows through",
			defaults: m{"MaxRetries": 1}, file: m{"MaxRetries": 2}, env: m{"MaxRetries": 7},
			flags: m{"Verbose": true},
			want:  Config{Verbose: true, MaxRetries: 7},
		},
		{
			name:     "fields independent",
			defaults: m{"Verbose": false, "MaxRetries": 1, "LogLevel": "debug", "TimeoutSec": 10},
			file:     m{"Verbose": false, "MaxRetries": 2, "LogLevel": "warn"},
			env:      m{"Verbose": false, "MaxRetries": 5},
			flags:    m{"Verbose": true},
			want:     Config{Verbose: true, MaxRetries: 5, LogLevel: "warn", TimeoutSec: 10},
		},
		{
			name:     "nonzero zero nonzero",
			defaults: m{"TimeoutSec": 5}, file: m{"TimeoutSec": 0}, env: nil, flags: m{"TimeoutSec": 9},
			want: Config{TimeoutSec: 9},
		},
		{
			name:     "negative ints not validated",
			defaults: m{"MaxRetries": -1, "TimeoutSec": -5},
			want:     Config{MaxRetries: -1, TimeoutSec: -5},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Resolve(tt.defaults, tt.file, tt.env, tt.flags)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestValidLogLevels(t *testing.T) {
	for _, lvl := range []string{"debug", "info", "warn", "error", ""} {
		got, err := Resolve(nil, nil, nil, m{"LogLevel": lvl})
		if err != nil {
			t.Errorf("level %q: unexpected error %v", lvl, err)
		}
		if got.LogLevel != lvl {
			t.Errorf("level %q: got %q", lvl, got.LogLevel)
		}
	}
}

func TestInvalidLogLevels(t *testing.T) {
	for _, lvl := range []string{"trace", "INFO", " info", "warning", "fatal"} {
		got, err := Resolve(m{"MaxRetries": 4}, nil, nil, m{"LogLevel": lvl})
		if !errors.Is(err, ErrInvalidLogLevel) {
			t.Errorf("level %q: err = %v, want ErrInvalidLogLevel", lvl, err)
			continue
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("%q", lvl)) {
			t.Errorf("level %q: message %q lacks quoted value", lvl, err)
		}
		if got != (Config{}) {
			t.Errorf("level %q: got %+v on error, want zero Config", lvl, got)
		}
	}
}

func TestValidationOnlyEffective(t *testing.T) {
	tests := []struct {
		name                       string
		defaults, file, env, flags m
		wantErr                    bool
		wantLevel                  string
	}{
		{name: "invalid defaults, no override", defaults: m{"LogLevel": "trace"}, wantErr: true},
		{name: "invalid defaults overridden", defaults: m{"LogLevel": "trace"}, flags: m{"LogLevel": "info"}, wantLevel: "info"},
		{name: "invalid file overridden", file: m{"LogLevel": "trace"}, env: m{"LogLevel": "warn"}, wantLevel: "warn"},
		{name: "invalid env overridden", env: m{"LogLevel": "trace"}, flags: m{"LogLevel": "error"}, wantLevel: "error"},
		{name: "invalid overridden by empty", defaults: m{"LogLevel": "trace"}, file: m{"LogLevel": ""}, wantLevel: ""},
		{name: "invalid env, absent flags", env: m{"LogLevel": "bogus"}, flags: m{"Verbose": true}, wantErr: true},
		{name: "never presented", defaults: m{"Verbose": true}, wantLevel: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Resolve(tt.defaults, tt.file, tt.env, tt.flags)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidLogLevel) {
					t.Fatalf("err = %v, want ErrInvalidLogLevel", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.LogLevel != tt.wantLevel {
				t.Fatalf("LogLevel = %q, want %q", got.LogLevel, tt.wantLevel)
			}
		})
	}
}

func mustPanic(t *testing.T, substr string, f func()) {
	t.Helper()
	defer func() {
		t.Helper()
		r := recover()
		if r == nil {
			t.Fatalf("expected panic containing %q, got none", substr)
		}
		msg := fmt.Sprint(r)
		if !strings.Contains(msg, substr) {
			t.Fatalf("panic %q does not contain %q", msg, substr)
		}
	}()
	f()
}

type myInt int

func TestWrongTypePanics(t *testing.T) {
	tests := []struct {
		key string
		val any
	}{
		{"Verbose", "true"},
		{"Verbose", 1},
		{"MaxRetries", int64(3)},
		{"MaxRetries", 3.0},
		{"MaxRetries", myInt(3)},
		{"MaxRetries", nil},
		{"LogLevel", 1},
		{"LogLevel", nil},
		{"TimeoutSec", "30"},
		{"TimeoutSec", uint(5)},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s=%T", tt.key, tt.val), func(t *testing.T) {
			mustPanic(t, fmt.Sprintf("key %q", tt.key), func() {
				_, _ = Resolve(nil, nil, nil, m{tt.key: tt.val})
			})
		})
	}
}

func TestWrongTypeInOverriddenLayerPanics(t *testing.T) {
	mustPanic(t, `layer "defaults"`, func() {
		_, _ = Resolve(m{"MaxRetries": "x"}, nil, nil, m{"MaxRetries": 1})
	})
}

func TestWrongTypeNamesLayer(t *testing.T) {
	bad := m{"Verbose": "yes"}
	cases := map[string]func(){
		"defaults": func() { _, _ = Resolve(bad, nil, nil, nil) },
		"file":     func() { _, _ = Resolve(nil, bad, nil, nil) },
		"env":      func() { _, _ = Resolve(nil, nil, bad, nil) },
		"flags":    func() { _, _ = Resolve(nil, nil, nil, bad) },
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			mustPanic(t, fmt.Sprintf("layer %q", name), f)
		})
	}
}

func TestUnknownKeyPanics(t *testing.T) {
	for _, k := range []string{"Verbos", "verbose", "loglevel", ""} {
		t.Run(k, func(t *testing.T) {
			mustPanic(t, fmt.Sprintf("unknown key %q", k), func() {
				_, _ = Resolve(nil, m{k: true}, nil, nil)
			})
		})
	}
}

func TestFieldTableMatchesStruct(t *testing.T) {
	typ := reflect.TypeFor[Config]()
	if len(fields) != typ.NumField() {
		t.Fatalf("fields table has %d entries, Config has %d fields", len(fields), typ.NumField())
	}
	for i := range typ.NumField() {
		sf := typ.Field(i)
		n := 0
		for _, f := range fields {
			if f.key == sf.Name {
				n++
				if f.typeName != sf.Type.String() {
					t.Errorf("field %s: descriptor type %s, struct type %s", sf.Name, f.typeName, sf.Type)
				}
			}
		}
		if n != 1 {
			t.Errorf("field %s: %d descriptors, want 1", sf.Name, n)
		}
	}
	// Each descriptor must write the struct field of the same name.
	samples := map[string]any{"bool": true, "int": 42, "string": "warn"}
	for _, f := range fields {
		var c Config
		v := samples[f.typeName]
		if !f.set(&c, v) {
			t.Fatalf("descriptor %s rejected sample %#v", f.key, v)
		}
		var want Config
		reflect.ValueOf(&want).Elem().FieldByName(f.key).Set(reflect.ValueOf(v))
		if c != want {
			t.Errorf("descriptor %s: got %+v, want %+v", f.key, c, want)
		}
	}
}

func TestInputsNotMutated(t *testing.T) {
	d := m{"Verbose": true, "MaxRetries": 3}
	f := m{"LogLevel": "info"}
	e := m{"MaxRetries": 0}
	fl := m{"TimeoutSec": 9}
	before := []m{maps.Clone(d), maps.Clone(f), maps.Clone(e), maps.Clone(fl)}
	if _, err := Resolve(d, f, e, fl); err != nil {
		t.Fatal(err)
	}
	after := []m{d, f, e, fl}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("inputs mutated: before %v, after %v", before, after)
	}
}

func TestConcurrentResolve(t *testing.T) {
	d := m{"Verbose": true, "MaxRetries": 3, "LogLevel": "info", "TimeoutSec": 30}
	fl := m{"LogLevel": "debug"}
	want := Config{Verbose: true, MaxRetries: 3, LogLevel: "debug", TimeoutSec: 30}
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			for range 100 {
				got, err := Resolve(d, nil, nil, fl)
				if err != nil || got != want {
					t.Errorf("got %+v, %v", got, err)
					return
				}
			}
		})
	}
	wg.Wait()
}
