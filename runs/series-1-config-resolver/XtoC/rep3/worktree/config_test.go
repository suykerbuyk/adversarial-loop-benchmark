package config

import (
	"errors"
	"strings"
	"testing"
)

func TestEmptyAndNilLayers(t *testing.T) {
	got, err := Resolve(nil, map[string]any{}, nil, nil)
	if err != nil || got != (Config{}) {
		t.Fatalf("Resolve empty layers = (%+v, %v), want zero config and nil", got, err)
	}
}

func TestLogLevelValidation(t *testing.T) {
	for _, level := range []string{"", "debug", "info", "warn", "error"} {
		got, err := Resolve(nil, nil, nil, map[string]any{"LogLevel": level})
		if err != nil || got.LogLevel != level {
			t.Errorf("level %q: got (%+v, %v)", level, got, err)
		}
	}
	for _, level := range []string{"trace", "INFO", " info", "warning"} {
		got, err := Resolve(nil, nil, nil, map[string]any{"LogLevel": level})
		if !errors.Is(err, ErrInvalidLogLevel) || got != (Config{}) {
			t.Errorf("invalid level %q: got (%+v, %v), want zero config and wrapped sentinel", level, got, err)
		}
	}
}

func TestOverriddenInvalidLogLevelIsNotValidated(t *testing.T) {
	got, err := Resolve(map[string]any{"LogLevel": "trace"}, nil, nil, map[string]any{"LogLevel": "info"})
	if err != nil || got.LogLevel != "info" {
		t.Fatalf("got (%+v, %v), want info and nil", got, err)
	}
}

func TestZeroValuesOverride(t *testing.T) {
	got, err := Resolve(map[string]any{"Verbose": true, "MaxRetries": 3, "LogLevel": "info"}, nil, nil,
		map[string]any{"Verbose": false, "MaxRetries": 0, "LogLevel": ""})
	want := Config{}
	if err != nil || got != want {
		t.Fatalf("got (%+v, %v), want (%+v, nil)", got, err, want)
	}
}

func TestWrongTypesPanicEvenWhenOverridden(t *testing.T) {
	cases := []struct {
		key, expected string
		bad           any
		higher        any
	}{
		{"Verbose", "bool", "true", true},
		{"MaxRetries", "int", int64(2), 3},
		{"LogLevel", "string", nil, "info"},
		{"TimeoutSec", "int", 1.5, 10},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			defer func() {
				recovered := recover()
				if recovered == nil {
					t.Fatal("expected panic")
				}
				if !errors.Is(recovered.(error), ErrWrongType) {
					t.Errorf("panic = %v, want ErrWrongType", recovered)
				}
				message := recovered.(error).Error()
				for _, part := range []string{tc.key, "defaults", tc.expected} {
					if !strings.Contains(message, part) {
						t.Errorf("panic %q does not contain %q", message, part)
					}
				}
			}()
			_, _ = Resolve(map[string]any{tc.key: tc.bad}, nil, nil, map[string]any{tc.key: tc.higher})
		})
	}
}

func TestUnknownKeyPanicsWithLayer(t *testing.T) {
	for _, layer := range []struct {
		name string
		key  string
		args [4]map[string]any
	}{
		{"defaults", "verbose", [4]map[string]any{{"verbose": true}, nil, nil, nil}},
		{"file", "Loglevel", [4]map[string]any{nil, {"Loglevel": "info"}, nil, nil}},
		{"env", "Extra", [4]map[string]any{nil, nil, {"Extra": 1}, nil}},
		{"flags", "Unknown", [4]map[string]any{nil, nil, nil, {"Unknown": true}}},
	} {
		t.Run(layer.name, func(t *testing.T) {
			defer func() {
				recovered := recover()
				if recovered == nil {
					t.Fatal("expected panic")
				}
				message := recovered.(error).Error()
				if !strings.Contains(message, layer.name) || !strings.Contains(message, layer.key) {
					t.Errorf("panic %q does not identify key %q and layer %q", message, layer.key, layer.name)
				}
				if !errors.Is(recovered.(error), ErrUnsupportedKey) {
					t.Errorf("panic = %v, want ErrUnsupportedKey", recovered)
				}
			}()
			_, _ = Resolve(layer.args[0], layer.args[1], layer.args[2], layer.args[3])
		})
	}
}

func TestUnknownKeyPanicChoiceIsDeterministic(t *testing.T) {
	defer func() {
		recovered := recover()
		if recovered == nil || !strings.Contains(recovered.(error).Error(), `"A"`) {
			t.Fatalf("panic = %v, want deterministic first key A", recovered)
		}
	}()
	_, _ = Resolve(map[string]any{"Z": 1, "A": 2}, nil, nil, nil)
}

func TestInvalidLogLevelNamesWinningLayer(t *testing.T) {
	_, err := Resolve(nil, map[string]any{"LogLevel": "trace"}, nil, nil)
	if !errors.Is(err, ErrInvalidLogLevel) || !strings.Contains(err.Error(), `layer "file"`) {
		t.Fatalf("error = %v, want invalid-level error naming file layer", err)
	}
}

func TestInputsAreNotMutated(t *testing.T) {
	defaults := map[string]any{"Verbose": true}
	flags := map[string]any{"MaxRetries": 0}
	_, err := Resolve(defaults, nil, nil, flags)
	if err != nil || len(defaults) != 1 || defaults["Verbose"] != true || len(flags) != 1 || flags["MaxRetries"] != 0 {
		t.Fatalf("input maps changed or resolution failed: defaults=%v flags=%v err=%v", defaults, flags, err)
	}
}
