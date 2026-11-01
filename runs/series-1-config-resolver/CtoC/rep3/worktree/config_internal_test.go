package config

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
)

type m = map[string]any

func TestResolveMerge(t *testing.T) {
	tests := []struct {
		name                       string
		defaults, file, env, flags m
		want                       Config
	}{
		{name: "all nil", want: Config{}},
		{name: "all empty", defaults: m{}, file: m{}, env: m{}, flags: m{}, want: Config{}},
		{
			name:     "zero values from file override",
			defaults: m{"Verbose": true, "MaxRetries": 3, "LogLevel": "info", "TimeoutSec": 9},
			file:     m{"Verbose": false, "MaxRetries": 0, "LogLevel": "", "TimeoutSec": 0},
			want:     Config{},
		},
		{
			name:     "zero values from env override",
			defaults: m{"Verbose": true, "MaxRetries": 3, "LogLevel": "info", "TimeoutSec": 9},
			file:     m{"Verbose": true, "MaxRetries": 4, "LogLevel": "warn", "TimeoutSec": 8},
			env:      m{"Verbose": false, "MaxRetries": 0, "LogLevel": "", "TimeoutSec": 0},
			want:     Config{},
		},
		{
			name:     "zero values from flags override",
			defaults: m{"Verbose": true, "MaxRetries": 3, "LogLevel": "info", "TimeoutSec": 9},
			env:      m{"Verbose": true, "MaxRetries": 4, "LogLevel": "warn", "TimeoutSec": 8},
			flags:    m{"Verbose": false, "MaxRetries": 0, "LogLevel": "", "TimeoutSec": 0},
			want:     Config{},
		},
		{
			name:     "defaults show through gaps",
			defaults: m{"TimeoutSec": 7},
			file:     m{"Verbose": true},
			env:      m{},
			flags:    m{"MaxRetries": 2},
			want:     Config{Verbose: true, MaxRetries: 2, TimeoutSec: 7},
		},
		{
			name:     "middle layer wins when flags absent",
			defaults: m{"LogLevel": "info"},
			file:     m{"LogLevel": "warn"},
			env:      m{"LogLevel": "error"},
			flags:    m{"Verbose": true},
			want:     Config{Verbose: true, LogLevel: "error"},
		},
		{
			name:     "each field won by a different layer",
			defaults: m{"Verbose": true, "MaxRetries": 1, "LogLevel": "info", "TimeoutSec": 1},
			file:     m{"MaxRetries": 2, "LogLevel": "warn", "TimeoutSec": 2},
			env:      m{"LogLevel": "debug", "TimeoutSec": 3},
			flags:    m{"TimeoutSec": 4},
			want:     Config{Verbose: true, MaxRetries: 2, LogLevel: "debug", TimeoutSec: 4},
		},
		{
			name:     "non-LogLevel fields are not validated",
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

func TestLegalLogLevels(t *testing.T) {
	for _, lvl := range []string{"", "debug", "info", "warn", "error"} {
		got, err := Resolve(nil, nil, nil, m{"LogLevel": lvl})
		if err != nil {
			t.Errorf("level %q: unexpected error %v", lvl, err)
		}
		if got.LogLevel != lvl {
			t.Errorf("level %q: got %q", lvl, got.LogLevel)
		}
	}
}

func TestInvalidLogLevelsReturnEffectiveConfig(t *testing.T) {
	for _, lvl := range []string{"trace", "INFO", " info", "warning"} {
		t.Run(lvl, func(t *testing.T) {
			got, err := Resolve(
				m{"Verbose": true, "MaxRetries": 1, "TimeoutSec": 1},
				m{"MaxRetries": 2},
				m{"LogLevel": lvl},
				m{"TimeoutSec": 4},
			)
			if !errors.Is(err, ErrInvalidLogLevel) {
				t.Fatalf("err = %v, want ErrInvalidLogLevel", err)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("%q", lvl)) {
				t.Errorf("error %q does not mention quoted value %q", err, lvl)
			}
			want := Config{Verbose: true, MaxRetries: 2, LogLevel: lvl, TimeoutSec: 4}
			if got != want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
	}
}

func TestOnlyEffectiveLogLevelValidated(t *testing.T) {
	tests := []struct {
		name                       string
		defaults, file, env, flags m
		wantErr                    bool
	}{
		{name: "invalid defaults overridden", defaults: m{"LogLevel": "trace"}, flags: m{"LogLevel": "info"}},
		{name: "invalid file overridden", file: m{"LogLevel": "trace"}, flags: m{"LogLevel": "debug"}},
		{name: "invalid env overridden", env: m{"LogLevel": "trace"}, flags: m{"LogLevel": "warn"}},
		{name: "invalid overridden by empty", defaults: m{"LogLevel": "trace"}, file: m{"LogLevel": ""}},
		{name: "valid overridden by invalid", defaults: m{"LogLevel": "info"}, env: m{"LogLevel": "trace"}, wantErr: true},
		{name: "invalid shows through", defaults: m{"LogLevel": "trace"}, flags: m{"Verbose": true}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Resolve(tt.defaults, tt.file, tt.env, tt.flags)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidLogLevel) {
					t.Fatalf("err = %v, want ErrInvalidLogLevel", err)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

type namedLevel string

// recoverPanic runs f and returns the recovered panic value formatted as a
// string, failing the test if f does not panic.
func recoverPanic(t *testing.T, f func()) (msg string) {
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

func TestPanics(t *testing.T) {
	tests := []struct {
		name                       string
		defaults, file, env, flags m
		wantInMsg                  string
	}{
		{name: "int64 MaxRetries", env: m{"MaxRetries": int64(3)}, wantInMsg: "MaxRetries"},
		{name: "float64 TimeoutSec", file: m{"TimeoutSec": 1.5}, wantInMsg: "TimeoutSec"},
		{name: "string Verbose", flags: m{"Verbose": "true"}, wantInMsg: "Verbose"},
		{name: "int LogLevel", defaults: m{"LogLevel": 1}, wantInMsg: "LogLevel"},
		{name: "named string LogLevel", defaults: m{"LogLevel": namedLevel("info")}, wantInMsg: "LogLevel"},
		{name: "uint MaxRetries", defaults: m{"MaxRetries": uint(1)}, wantInMsg: "MaxRetries"},
		{name: "nil value", file: m{"Verbose": nil}, wantInMsg: "Verbose"},
		{name: "unknown key", flags: m{"loglevel": "info"}, wantInMsg: "loglevel"},
		{
			name:      "wrong type in overridden lower layer",
			defaults:  m{"MaxRetries": "3"},
			flags:     m{"MaxRetries": 3},
			wantInMsg: "MaxRetries",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := recoverPanic(t, func() {
				_, _ = Resolve(tt.defaults, tt.file, tt.env, tt.flags)
			})
			if !strings.Contains(msg, tt.wantInMsg) {
				t.Errorf("panic message %q does not contain %q", msg, tt.wantInMsg)
			}
		})
	}
}

func TestPanicMessageOmitsValue(t *testing.T) {
	msg := recoverPanic(t, func() {
		_, _ = Resolve(nil, nil, m{"MaxRetries": int64(987654)}, nil)
	})
	if strings.Contains(msg, "987654") {
		t.Errorf("panic message %q leaks the value", msg)
	}
	if !strings.Contains(msg, "int64") {
		t.Errorf("panic message %q does not name the offending type", msg)
	}
}

func TestInputsNotMutated(t *testing.T) {
	defaults := m{"Verbose": true, "MaxRetries": 3, "LogLevel": "info", "TimeoutSec": 9}
	file := m{"LogLevel": ""}
	env := m{"MaxRetries": 0}
	flags := m{"Verbose": false}
	layers := []m{defaults, file, env, flags}
	var before []m
	for _, l := range layers {
		before = append(before, maps.Clone(l))
	}
	if _, err := Resolve(defaults, file, env, flags); err != nil {
		t.Fatal(err)
	}
	for i, l := range layers {
		if !maps.Equal(l, before[i]) {
			t.Errorf("layer %d mutated: got %v, want %v", i, l, before[i])
		}
	}
}

func TestBindingsCoverConfigFields(t *testing.T) {
	typ := reflect.TypeFor[Config]()
	if len(bindings) != typ.NumField() {
		t.Fatalf("len(bindings) = %d, Config has %d fields", len(bindings), typ.NumField())
	}
	if len(knownKeys) != len(bindings) {
		t.Fatalf("len(knownKeys) = %d, len(bindings) = %d", len(knownKeys), len(bindings))
	}
	for i := range typ.NumField() {
		if got, want := bindings[i].key, typ.Field(i).Name; got != want {
			t.Errorf("bindings[%d].key = %q, want %q (field declaration order)", i, got, want)
		}
	}
}

func TestBindingsWiring(t *testing.T) {
	typ := reflect.TypeFor[Config]()
	for i := range typ.NumField() {
		f := typ.Field(i)
		// Build the sample with the field's exact type (named types included),
		// so the binding's exact type assertion accepts it.
		sample := reflect.New(f.Type).Elem()
		switch f.Type.Kind() {
		case reflect.Bool:
			sample.SetBool(true)
		case reflect.Int:
			sample.SetInt(int64(1000 + i))
		case reflect.String:
			sample.SetString("x" + f.Name)
		default:
			t.Fatalf("field %s: unsupported kind %s; extend this test", f.Name, f.Type.Kind())
		}
		v := sample.Interface()
		idx := slices.IndexFunc(bindings, func(b binding) bool { return b.key == f.Name })
		if idx < 0 {
			t.Fatalf("no binding for field %s", f.Name)
		}
		b := bindings[idx]
		var c Config
		b.set(&c, "test", v)
		rv := reflect.ValueOf(c)
		for j := range typ.NumField() {
			got := rv.Field(j).Interface()
			if j == i {
				if got != v {
					t.Errorf("binding %q: field %s = %v, want %v", f.Name, f.Name, got, v)
				}
			} else if !rv.Field(j).IsZero() {
				t.Errorf("binding %q also changed field %s to %v", f.Name, typ.Field(j).Name, got)
			}
		}
	}
}

func TestResolveConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			want := Config{Verbose: true, MaxRetries: i, LogLevel: "info", TimeoutSec: i + 1}
			got, err := Resolve(m{"Verbose": true, "LogLevel": "debug"}, m{"MaxRetries": i},
				m{"LogLevel": "info"}, m{"TimeoutSec": i + 1})
			if err != nil || got != want {
				t.Errorf("goroutine %d: got %+v, %v; want %+v", i, got, err, want)
			}
		}()
	}
	wg.Wait()
}

func TestPanicIsDeterministic(t *testing.T) {
	cases := []struct {
		name  string
		layer m
		want  string
	}{
		{
			name:  "unknown keys reported first, sorted",
			layer: m{"zeta": 1, "bogus": 1, "MaxRetries": "x", "Verbose": 1},
			want:  `config: layer "file": unknown key(s) "bogus", "zeta"`,
		},
		{
			name:  "first wrong type in field order",
			layer: m{"TimeoutSec": "x", "MaxRetries": "x", "Verbose": 1},
			want:  `config: layer "file", key "Verbose": value has type int, want bool`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for range 50 {
				msg := recoverPanic(t, func() { _, _ = Resolve(nil, tc.layer, nil, nil) })
				if msg != tc.want {
					t.Fatalf("panic message = %q, want %q", msg, tc.want)
				}
			}
		})
	}
}

func TestPanicValueIsError(t *testing.T) {
	defer func() {
		if _, ok := recover().(error); !ok {
			t.Error("panic value is not an error")
		}
	}()
	_, _ = Resolve(m{"Verbose": 1}, nil, nil, nil)
}

func TestSingleValidationErrorUnwraps(t *testing.T) {
	_, err := Resolve(nil, nil, nil, m{"LogLevel": "trace"})
	if got := errors.Unwrap(err); got != ErrInvalidLogLevel {
		t.Fatalf("errors.Unwrap(err) = %v, want ErrInvalidLogLevel", got)
	}
}

func TestIndexBindingsRejectsDuplicates(t *testing.T) {
	dup := []binding{bindings[0], bindings[0]}
	msg := recoverPanic(t, func() { indexBindings(dup) })
	if !strings.Contains(msg, bindings[0].key) {
		t.Errorf("panic message %q does not name the duplicate key", msg)
	}
}

func TestValidateJoinsMultipleErrors(t *testing.T) {
	errA := errors.New("a")
	errB := errors.New("b")
	bs := []binding{
		{key: "A", validate: func(*Config) error { return errA }},
		{key: "N"}, // no validator
		{key: "B", validate: func(*Config) error { return errB }},
	}
	err := validate(&Config{}, bs)
	if !errors.Is(err, errA) || !errors.Is(err, errB) {
		t.Fatalf("err = %v, want both errA and errB", err)
	}
	if got, want := err.Error(), "a\nb"; got != want {
		t.Errorf("err.Error() = %q, want %q (validator order)", got, want)
	}
	if err := validate(&Config{}, bs[1:2]); err != nil {
		t.Errorf("no validators: err = %v, want nil", err)
	}
}

func TestEchoedTextIsTruncated(t *testing.T) {
	long := "secret=" + strings.Repeat("x", 100) + "TAIL"

	_, err := Resolve(nil, nil, nil, m{"LogLevel": long})
	if !errors.Is(err, ErrInvalidLogLevel) {
		t.Fatalf("err = %v, want ErrInvalidLogLevel", err)
	}
	if strings.Contains(err.Error(), "TAIL") {
		t.Errorf("error %q echoes the full value", err)
	}
	if !strings.Contains(err.Error(), "...") {
		t.Errorf("error %q lacks the truncation marker", err)
	}

	msg := recoverPanic(t, func() { _, _ = Resolve(nil, m{long: 1}, nil, nil) })
	if strings.Contains(msg, "TAIL") {
		t.Errorf("panic %q echoes the full key", msg)
	}

	many := m{}
	for i := range maxEchoKeys + 3 {
		many[fmt.Sprintf("k%d", i)] = 1
	}
	msg = recoverPanic(t, func() { _, _ = Resolve(many, nil, nil, nil) })
	want := `config: layer "defaults": unknown key(s) "k0", "k1", "k2", "k3", "k4", and 3 more`
	if msg != want {
		t.Errorf("panic = %q, want %q", msg, want)
	}
}

func TestQuoteTrunc(t *testing.T) {
	exact := strings.Repeat("é", maxEchoRunes)
	if got, want := quoteTrunc(exact), fmt.Sprintf("%q", exact); got != want {
		t.Errorf("at limit: got %s, want %s", got, want)
	}
	over := exact + "z"
	if got, want := quoteTrunc(over), fmt.Sprintf("%q...", exact); got != want {
		t.Errorf("over limit: got %s, want %s", got, want)
	}
}
