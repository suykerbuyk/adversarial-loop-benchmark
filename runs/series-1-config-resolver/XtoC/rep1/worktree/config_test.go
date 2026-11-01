package config

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

type namedLevel string

func TestFieldDescriptorsMatchConfig(t *testing.T) {
	typ := reflect.TypeOf(Config{})
	seen := make(map[string]bool)
	for _, descriptor := range fields {
		field, ok := typ.FieldByName(descriptor.key)
		if !ok {
			t.Errorf("descriptor %q has no Config field", descriptor.key)
			continue
		}
		if field.Type != descriptor.typeOf {
			t.Errorf("descriptor %q type %v does not match Config type %v", descriptor.key, descriptor.typeOf, field.Type)
		}
		if seen[descriptor.key] {
			t.Errorf("duplicate descriptor %q", descriptor.key)
		}
		seen[descriptor.key] = true
	}
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if !seen[name] {
			t.Errorf("Config field %q has no descriptor", name)
		}
	}
}

func TestResolveEffectiveLogLevels(t *testing.T) {
	for _, level := range []string{"", "debug", "info", "warn", "error"} {
		t.Run("valid_"+level, func(t *testing.T) {
			got, err := Resolve(nil, nil, nil, map[string]any{"LogLevel": level})
			if err != nil || got.LogLevel != level {
				t.Fatalf("Resolve = (%+v, %v), want level %q and no error", got, err, level)
			}
		})
	}
	invalidLevels := []string{"INFO", " info", "info "}
	for i, level := range invalidLevels {
		layers := [4]map[string]any{}
		layers[i] = map[string]any{"LogLevel": level}
		_, err := Resolve(layers[0], layers[1], layers[2], layers[3])
		if !errors.Is(err, ErrInvalidLogLevel) {
			t.Errorf("invalid level %q at layer %d: error = %v", level, i, err)
		}
	}
	got, err := Resolve(map[string]any{"LogLevel": "bogus"}, nil, nil, map[string]any{"LogLevel": ""})
	if err != nil || got.LogLevel != "" {
		t.Fatalf("overridden invalid level: (%+v, %v)", got, err)
	}
}

func TestResolvePanicsOnInvalidEntries(t *testing.T) {
	cases := []struct {
		name  string
		value map[string]any
		layer int
		want  string
	}{
		{"bool nil", map[string]any{"Verbose": nil}, 0, "defaults"},
		{"named string", map[string]any{"LogLevel": namedLevel("info")}, 1, "file"},
		{"int32", map[string]any{"MaxRetries": int32(1)}, 2, "env"},
		{"int64 hidden", map[string]any{"TimeoutSec": int64(1)}, 0, "defaults"},
		{"float64", map[string]any{"MaxRetries": float64(1)}, 3, "flags"},
		{"unknown", map[string]any{"Loglevel": "info"}, 2, "env"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			layers := [4]map[string]any{}
			layers[tc.layer] = tc.value
			if tc.name == "int64 hidden" {
				layers[3] = map[string]any{"TimeoutSec": 4}
			}
			defer func() {
				recovered := recover()
				if recovered == nil {
					t.Fatal("Resolve did not panic")
				}
				message := recovered.(string)
				if !strings.Contains(message, tc.want) {
					t.Errorf("panic %q does not identify layer %q", message, tc.want)
				}
			}()
			_, _ = Resolve(layers[0], layers[1], layers[2], layers[3])
		})
	}
}

func TestResolveReturnsResolvedConfigWithInvalidLevel(t *testing.T) {
	want := Config{Verbose: true, MaxRetries: 7, LogLevel: "trace", TimeoutSec: 9}
	got, err := Resolve(map[string]any{
		"Verbose": true, "MaxRetries": 7, "LogLevel": "trace", "TimeoutSec": 9,
	}, nil, nil, nil)
	if got != want || !errors.Is(err, ErrInvalidLogLevel) {
		t.Fatalf("Resolve = (%+v, %v), want (%+v, ErrInvalidLogLevel)", got, err, want)
	}
}

func TestResolveDoesNotMutateInputs(t *testing.T) {
	defaults := map[string]any{"Verbose": true, "MaxRetries": 3}
	flags := map[string]any{"Verbose": false}
	defaultsBefore := map[string]any{"Verbose": true, "MaxRetries": 3}
	flagsBefore := map[string]any{"Verbose": false}
	if _, err := Resolve(defaults, nil, nil, flags); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(defaults, defaultsBefore) || !reflect.DeepEqual(flags, flagsBefore) {
		t.Fatalf("Resolve mutated inputs: defaults=%v flags=%v", defaults, flags)
	}
}

func TestResolveAbsentLayersReturnZeroConfig(t *testing.T) {
	got, err := Resolve(nil, map[string]any{}, nil, nil)
	if err != nil || got != (Config{}) {
		t.Fatalf("Resolve = (%+v, %v), want zero Config and no error", got, err)
	}
}
