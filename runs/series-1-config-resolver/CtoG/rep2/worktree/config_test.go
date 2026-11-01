package config

import (
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"
)

type layers [4]map[string]any

func (l layers) resolve() (Config, error) { return Resolve(l[0], l[1], l[2], l[3]) }

func TestResolveTable(t *testing.T) {
	tests := []struct {
		name    string
		in      layers
		want    Config
		wantErr bool
	}{
		{name: "all nil", in: layers{}, want: Config{}},
		{name: "all empty", in: layers{{}, {}, {}, {}}, want: Config{}},
		{
			name: "zero values override at every boundary",
			in: layers{
				{"Verbose": true, "MaxRetries": 1, "LogLevel": "info", "TimeoutSec": 1},
				{"Verbose": false, "MaxRetries": 0, "LogLevel": "", "TimeoutSec": 0},
				{"Verbose": true, "MaxRetries": 2, "LogLevel": "warn", "TimeoutSec": 2},
				{"Verbose": false, "MaxRetries": 0, "LogLevel": "", "TimeoutSec": 0},
			},
			want: Config{},
		},
		{
			name: "zero in env over non-zero in file",
			in: layers{nil,
				{"Verbose": true, "MaxRetries": 7, "LogLevel": "debug", "TimeoutSec": 9},
				{"Verbose": false, "MaxRetries": 0, "LogLevel": "", "TimeoutSec": 0},
				nil},
			want: Config{},
		},
		{
			name: "non-zero over zero",
			in: layers{
				{"Verbose": false, "MaxRetries": 0, "LogLevel": "", "TimeoutSec": 0},
				nil, nil,
				{"Verbose": true, "MaxRetries": 4, "LogLevel": "error", "TimeoutSec": 8},
			},
			want: Config{Verbose: true, MaxRetries: 4, LogLevel: "error", TimeoutSec: 8},
		},
		{
			name: "middle layer shows through flags",
			in:   layers{nil, nil, {"MaxRetries": 6}, {"Verbose": true}},
			want: Config{Verbose: true, MaxRetries: 6},
		},
		{
			name: "mixed sources",
			in: layers{
				{"Verbose": true},
				{"MaxRetries": 2},
				{"LogLevel": "warn"},
				{"TimeoutSec": 10},
			},
			want: Config{Verbose: true, MaxRetries: 2, LogLevel: "warn", TimeoutSec: 10},
		},
		{name: "overridden invalid level", in: layers{{"LogLevel": "trace"}, nil, nil, {"LogLevel": "info"}}, want: Config{LogLevel: "info"}},
		{name: "overridden invalid by empty", in: layers{nil, {"LogLevel": "bogus"}, {"LogLevel": ""}, nil}, want: Config{}},
		{name: "valid overridden by invalid", in: layers{{"LogLevel": "info"}, nil, {"LogLevel": "bad"}, nil}, wantErr: true},
		{name: "negative and large ints", in: layers{{"MaxRetries": -1, "TimeoutSec": 1 << 40}}, want: Config{MaxRetries: -1, TimeoutSec: 1 << 40}},
	}
	for _, level := range []string{"debug", "info", "warn", "error", ""} {
		tests = append(tests, struct {
			name    string
			in      layers
			want    Config
			wantErr bool
		}{name: "legal level " + fmt.Sprintf("%q", level), in: layers{nil, nil, nil, {"LogLevel": level}}, want: Config{LogLevel: level}})
	}
	for _, level := range []string{"trace", "INFO", " info", "warning", "\x00"} {
		tests = append(tests, struct {
			name    string
			in      layers
			want    Config
			wantErr bool
		}{name: "illegal level " + fmt.Sprintf("%q", level), in: layers{{"Verbose": true}, nil, {"LogLevel": level}, nil}, wantErr: true})
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.in.resolve()
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidLogLevel) {
					t.Fatalf("err = %v, want ErrInvalidLogLevel", err)
				}
				if got != (Config{}) {
					t.Fatalf("got %+v on error, want zero Config", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func mustPanic(t *testing.T, wantSubstr string, f func()) {
	t.Helper()
	defer func() {
		t.Helper()
		r := recover()
		if r == nil {
			t.Fatalf("expected panic containing %q", wantSubstr)
		}
		if msg := fmt.Sprint(r); !strings.Contains(msg, wantSubstr) {
			t.Fatalf("panic %q does not contain %q", msg, wantSubstr)
		}
	}()
	f()
}

func TestWrongTypePanics(t *testing.T) {
	cases := []struct {
		key   string
		value any
		want  string
	}{
		{"Verbose", "true", "want bool, got string"},
		{"MaxRetries", int64(3), "want int, got int64"},
		{"MaxRetries", 3.0, "want int, got float64"},
		{"LogLevel", 1, "want string, got int"},
		{"TimeoutSec", nil, "want int, got <nil>"},
	}
	names := []string{"defaults", "file", "env", "flags"}
	for _, c := range cases {
		for pos := range 4 {
			t.Run(fmt.Sprintf("%s/%s", c.key, names[pos]), func(t *testing.T) {
				var in layers
				in[pos] = map[string]any{c.key: c.value}
				mustPanic(t, fmt.Sprintf("layer %q: key %q: %s", names[pos], c.key, c.want), func() { _, _ = in.resolve() })
			})
		}
	}
}

func TestWrongTypeInOverriddenLayerPanics(t *testing.T) {
	mustPanic(t, `layer "defaults"`, func() {
		_, _ = Resolve(map[string]any{"MaxRetries": "3"}, nil, nil, map[string]any{"MaxRetries": 3})
	})
}

func TestWrongTypeWithInvalidLevelPanics(t *testing.T) {
	mustPanic(t, "want int", func() {
		_, _ = Resolve(nil, nil, map[string]any{"TimeoutSec": "x"}, map[string]any{"LogLevel": "trace"})
	})
}

func TestUnknownKeyPanics(t *testing.T) {
	mustPanic(t, `layer "env": unknown key(s) ["Loglevel"]`, func() {
		_, _ = Resolve(nil, nil, map[string]any{"Loglevel": "info"}, nil)
	})
	mustPanic(t, `"Timeout"`, func() {
		_, _ = Resolve(map[string]any{"Timeout": 3}, nil, nil, map[string]any{"TimeoutSec": 3})
	})
	for range 20 {
		mustPanic(t, `unknown key(s) ["A" "B" "C"]`, func() {
			_, _ = Resolve(nil, map[string]any{"C": 1, "A": 1, "B": 1, "Verbose": true}, nil, nil)
		})
	}
}

func TestInputsNotMutated(t *testing.T) {
	in := layers{
		{"Verbose": true, "MaxRetries": 1},
		{"LogLevel": "info"},
		{"TimeoutSec": 0},
		{"Verbose": false},
	}
	var before layers
	for i, m := range in {
		before[i] = maps.Clone(m)
	}
	if _, err := in.resolve(); err != nil {
		t.Fatal(err)
	}
	for i := range in {
		if !maps.Equal(in[i], before[i]) {
			t.Fatalf("layer %d mutated: %v -> %v", i, before[i], in[i])
		}
	}
}

func TestDeterministic(t *testing.T) {
	in := layers{{"Verbose": true, "LogLevel": "debug"}, {"MaxRetries": 2}, nil, {"TimeoutSec": 5}}
	first, err := in.resolve()
	if err != nil {
		t.Fatal(err)
	}
	for range 50 {
		got, err := in.resolve()
		if err != nil || got != first {
			t.Fatalf("got %+v, %v; want %+v", got, err, first)
		}
	}
}

func ExampleResolve() {
	cfg, err := Resolve(
		map[string]any{"Verbose": true, "MaxRetries": 3, "LogLevel": "info"},
		map[string]any{"LogLevel": ""},   // explicit empty level overrides "info"
		map[string]any{"MaxRetries": 0},  // explicit 0 overrides 3
		map[string]any{"Verbose": false}, // explicit false overrides true
	)
	fmt.Printf("%+v %v\n", cfg, err)
	// Output: {Verbose:false MaxRetries:0 LogLevel: TimeoutSec:0} <nil>
}
