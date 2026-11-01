package config

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestZeroAndLegalLogLevels(t *testing.T) {
	got, err := Resolve(nil, nil, nil, nil)
	if err != nil || got != (Config{}) {
		t.Fatalf("Resolve(nil layers) = %+v, %v; want zero Config, nil", got, err)
	}
	for _, level := range []string{"", "debug", "info", "warn", "error"} {
		got, err := Resolve(map[string]any{"LogLevel": level}, nil, nil, nil)
		if err != nil || got.LogLevel != level {
			t.Errorf("LogLevel %q: got %q, err %v", level, got.LogLevel, err)
		}
	}
}

func TestLogLevelValidationAppliesAfterOverrides(t *testing.T) {
	got, err := Resolve(map[string]any{"LogLevel": "trace"}, nil, nil, map[string]any{"LogLevel": "info"})
	if err != nil || got.LogLevel != "info" {
		t.Fatalf("overridden invalid level: got %+v, %v", got, err)
	}
	for _, level := range []string{"trace", "INFO", " info"} {
		_, err := Resolve(nil, nil, nil, map[string]any{"LogLevel": level})
		if !errors.Is(err, ErrInvalidLogLevel) || !strings.Contains(err.Error(), fmt.Sprintf("%q", level)) {
			t.Errorf("level %q: err %v should wrap sentinel and include value", level, err)
		}
	}
}

func TestWrongTypesPanic(t *testing.T) {
	cases := []struct {
		key   string
		value any
	}{
		{"Verbose", "true"}, {"MaxRetries", int64(1)}, {"LogLevel", 1}, {"TimeoutSec", float64(1)},
		{"MaxRetries", nil}, {"TimeoutSec", uint(1)}, {"LogLevel", namedLevel("info")},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			assertPanic(t, func() { _, _ = Resolve(map[string]any{tc.key: tc.value}, nil, nil, nil) })
		})
	}
	assertPanic(t, func() {
		_, _ = Resolve(map[string]any{"MaxRetries": "bad"}, nil, nil, map[string]any{"MaxRetries": 2})
	})
}

type namedLevel string

func TestUnknownKeysPanicInEveryLayer(t *testing.T) {
	layers := []struct {
		name string
		call func(map[string]any) (Config, error)
	}{
		{"defaults", func(m map[string]any) (Config, error) { return Resolve(m, nil, nil, nil) }},
		{"file", func(m map[string]any) (Config, error) { return Resolve(nil, m, nil, nil) }},
		{"env", func(m map[string]any) (Config, error) { return Resolve(nil, nil, m, nil) }},
		{"flags", func(m map[string]any) (Config, error) { return Resolve(nil, nil, nil, m) }},
	}
	for _, layer := range layers {
		t.Run(layer.name, func(t *testing.T) {
			defer func() {
				value := recover()
				if value == nil || !strings.Contains(fmt.Sprint(value), layer.name) || !strings.Contains(fmt.Sprint(value), "Unknown") {
					t.Fatalf("panic = %v, want layer and key", value)
				}
			}()
			_, _ = layer.call(map[string]any{"Unknown": true})
		})
	}
}

func TestInputsUnchangedAndConfigFieldsHaveDescriptors(t *testing.T) {
	input := map[string]any{"Verbose": true, "LogLevel": "warn"}
	wantInput := map[string]any{"Verbose": true, "LogLevel": "warn"}
	if _, err := Resolve(input, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(input, wantInput) {
		t.Fatalf("input mutated: got %#v, want %#v", input, wantInput)
	}
	typeOfConfig := reflect.TypeOf(Config{})
	for i := 0; i < typeOfConfig.NumField(); i++ {
		name := typeOfConfig.Field(i).Name
		if _, ok := fields[name]; !ok {
			t.Errorf("Config field %q has no descriptor", name)
		}
	}
}

func assertPanic(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	fn()
}
