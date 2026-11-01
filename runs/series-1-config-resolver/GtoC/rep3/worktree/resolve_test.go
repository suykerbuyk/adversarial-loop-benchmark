package config

import (
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestNothingPresentedIsZero(t *testing.T) {
	got, err := Resolve(nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != (Config{}) {
		t.Fatalf("got %+v, want zero Config", got)
	}
}

func TestNilAndEmptyMaps(t *testing.T) {
	got, err := Resolve(map[string]any{}, nil, map[string]any{}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != (Config{}) {
		t.Fatalf("got %+v, want zero Config", got)
	}
}

func TestEachLegalLogLevel(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "error", ""} {
		got, err := Resolve(nil, nil, nil, map[string]any{"LogLevel": level})
		if err != nil {
			t.Fatalf("LogLevel %q: unexpected error: %v", level, err)
		}
		if got.LogLevel != level {
			t.Fatalf("LogLevel = %q, want %q", got.LogLevel, level)
		}
	}
}

func TestInvalidLogLevelExact(t *testing.T) {
	for _, level := range []string{"INFO", "Warning", " info", "error ", "warning", "trace"} {
		got, err := Resolve(nil, nil, nil, map[string]any{"LogLevel": level})
		if !errors.Is(err, ErrInvalidLogLevel) {
			t.Fatalf("LogLevel %q: err = %v, want ErrInvalidLogLevel", level, err)
		}
		if errors.Unwrap(err) != ErrInvalidLogLevel {
			t.Fatalf("LogLevel %q: unwrap = %v, want sentinel", level, errors.Unwrap(err))
		}
		if got.LogLevel != level {
			t.Fatalf("LogLevel = %q, want %q", got.LogLevel, level)
		}
		if !strings.Contains(err.Error(), `"`+level+`"`) {
			t.Fatalf("error %q does not quote effective level %q", err, level)
		}
	}
}

func TestOverriddenInvalidLogLevelSkipped(t *testing.T) {
	got, err := Resolve(
		map[string]any{"LogLevel": "trace"},
		nil, nil,
		map[string]any{"LogLevel": "info"},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.LogLevel != "info" {
		t.Fatalf("LogLevel = %q, want info", got.LogLevel)
	}

	got, err = Resolve(
		map[string]any{"LogLevel": "trace"},
		map[string]any{"LogLevel": ""},
		nil, nil,
	)
	if err != nil {
		t.Fatalf("empty override: unexpected error: %v", err)
	}
	if got.LogLevel != "" {
		t.Fatalf("LogLevel = %q, want empty", got.LogLevel)
	}
}

func TestInvalidEffectiveReturnsResolvedConfig(t *testing.T) {
	got, err := Resolve(
		map[string]any{"MaxRetries": 7, "LogLevel": "nope-lower", "Verbose": true},
		nil, nil,
		map[string]any{"LogLevel": "trace"},
	)
	if !errors.Is(err, ErrInvalidLogLevel) {
		t.Fatalf("err = %v, want ErrInvalidLogLevel", err)
	}
	if errors.Unwrap(err) != ErrInvalidLogLevel {
		t.Fatalf("unwrap = %v, want sentinel", errors.Unwrap(err))
	}
	if strings.Contains(err.Error(), "nope-lower") {
		t.Fatalf("error %q includes overridden level", err)
	}
	want := Config{Verbose: true, MaxRetries: 7, LogLevel: "trace"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestLayerPairPrecedence(t *testing.T) {
	// Names match Resolve argument order: later index wins.
	names := []string{"defaults", "file", "env", "flags"}
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			t.Run(names[i]+"_vs_"+names[j], func(t *testing.T) {
				var layers [4]map[string]any
				layers[i] = map[string]any{"TimeoutSec": 10 + i}
				layers[j] = map[string]any{"TimeoutSec": 10 + j}
				got, err := Resolve(layers[0], layers[1], layers[2], layers[3])
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got.TimeoutSec != 10+j {
					t.Fatalf("TimeoutSec = %d, want %d (%s overrides %s)", got.TimeoutSec, 10+j, names[j], names[i])
				}
				if got != (Config{TimeoutSec: 10 + j}) {
					t.Fatalf("got %+v, sibling fields must stay zero", got)
				}
			})
		}
	}
}

func TestFileBelowEnv(t *testing.T) {
	got, err := Resolve(
		nil,
		map[string]any{"MaxRetries": 2, "Verbose": true},
		map[string]any{"MaxRetries": 3},
		nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := Config{Verbose: true, MaxRetries: 3}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestPerFieldPrecedence(t *testing.T) {
	got, err := Resolve(
		map[string]any{"Verbose": true, "MaxRetries": 3, "LogLevel": "info", "TimeoutSec": 1},
		map[string]any{"Verbose": false, "TimeoutSec": 9},
		map[string]any{"MaxRetries": 0},
		map[string]any{"LogLevel": "error"},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := Config{Verbose: false, MaxRetries: 0, LogLevel: "error", TimeoutSec: 9}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestHighestLayerZeroValues(t *testing.T) {
	got, err := Resolve(
		nil,
		map[string]any{"LogLevel": "debug"},
		map[string]any{"TimeoutSec": 10},
		map[string]any{"LogLevel": "", "TimeoutSec": 0},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := Config{LogLevel: "", TimeoutSec: 0}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestNegativeIntsAllowed(t *testing.T) {
	got, err := Resolve(nil, nil, map[string]any{"MaxRetries": -1, "TimeoutSec": -5}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.MaxRetries != -1 || got.TimeoutSec != -5 {
		t.Fatalf("got %+v, want negative ints preserved", got)
	}
}

type planted struct{ payload string }

func (p planted) String() string   { return p.payload }
func (p planted) GoString() string { return p.payload }

func TestWrongTypePanics(t *testing.T) {
	type Level string
	type MyInt int

	secret := planted{payload: "secret=do-not-log"}
	tests := []struct {
		name     string
		slot     int
		layer    string
		key      string
		values   map[string]any
		wantType string
		forbid   string
	}{
		{name: "verbose string", slot: 0, layer: "defaults", key: "Verbose", wantType: "bool", values: map[string]any{"Verbose": "true"}},
		{name: "verbose int", slot: 1, layer: "file", key: "Verbose", wantType: "bool", values: map[string]any{"Verbose": 1}},
		{name: "verbose nil", slot: 2, layer: "env", key: "Verbose", wantType: "bool", values: map[string]any{"Verbose": nil}},
		{name: "verbose planted", slot: 3, layer: "flags", key: "Verbose", wantType: "bool", forbid: "secret=", values: map[string]any{"Verbose": secret}},
		{name: "retries int64", slot: 0, layer: "defaults", key: "MaxRetries", wantType: "int", values: map[string]any{"MaxRetries": int64(1)}},
		{name: "retries float64", slot: 1, layer: "file", key: "MaxRetries", wantType: "int", values: map[string]any{"MaxRetries": float64(1)}},
		{name: "retries uint", slot: 2, layer: "env", key: "MaxRetries", wantType: "int", values: map[string]any{"MaxRetries": uint(1)}},
		{name: "retries named", slot: 3, layer: "flags", key: "MaxRetries", wantType: "int", values: map[string]any{"MaxRetries": MyInt(1)}},
		{name: "level bytes", slot: 0, layer: "defaults", key: "LogLevel", wantType: "string", values: map[string]any{"LogLevel": []byte("info")}},
		{name: "level named", slot: 1, layer: "file", key: "LogLevel", wantType: "string", values: map[string]any{"LogLevel": Level("info")}},
		{name: "timeout int32", slot: 2, layer: "env", key: "TimeoutSec", wantType: "int", values: map[string]any{"TimeoutSec": int32(1)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := mustPanic(t, func() {
				_, _ = Resolve(layerSlot(tt.slot, tt.values))
			})
			msg := err.Error()
			if !strings.Contains(msg, tt.layer) || !strings.Contains(msg, tt.key) {
				t.Fatalf("message %q missing layer %q or key %q", msg, tt.layer, tt.key)
			}
			if !strings.Contains(msg, "want "+tt.wantType) {
				t.Fatalf("message %q missing want %s", msg, tt.wantType)
			}
			if tt.forbid != "" && strings.Contains(msg, tt.forbid) {
				t.Fatalf("message %q contains planted payload %q", msg, tt.forbid)
			}
		})
	}
}

func TestWrongTypeLowerLayerPanics(t *testing.T) {
	err := mustPanic(t, func() {
		_, _ = Resolve(
			map[string]any{"LogLevel": 1},
			nil, nil,
			map[string]any{"LogLevel": "info"},
		)
	})
	msg := err.Error()
	if !strings.Contains(msg, "defaults") || !strings.Contains(msg, "LogLevel") {
		t.Fatalf("message %q missing layer or key", msg)
	}
}

func TestUnknownKeyPanics(t *testing.T) {
	tests := []struct {
		name  string
		slot  int
		layer string
		key   string
	}{
		{name: "wrong case loglevel", slot: 0, layer: "defaults", key: "loglevel"},
		{name: "wrong case verbose", slot: 1, layer: "file", key: "verbose"},
		{name: "empty key", slot: 2, layer: "env", key: ""},
		{name: "key with space", slot: 3, layer: "flags", key: "has space"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := mustPanic(t, func() {
				_, _ = Resolve(layerSlot(tt.slot, map[string]any{tt.key: 1}))
			})
			msg := err.Error()
			if !strings.Contains(msg, `layer "`+tt.layer+`"`) {
				t.Fatalf("message %q missing layer %q", msg, tt.layer)
			}
			if !strings.Contains(msg, `"`+tt.key+`"`) {
				t.Fatalf("message %q missing key %q", msg, tt.key)
			}
		})
	}
}

func TestUnknownKeysSortedStable(t *testing.T) {
	layer := map[string]any{"zeta": 1, "bogus": 2}
	const want = `config: layer "env": unknown key(s) "bogus", "zeta"`
	var first string
	for i := 0; i < 50; i++ {
		err := mustPanic(t, func() {
			_, _ = Resolve(nil, nil, layer, nil)
		})
		msg := err.Error()
		if i == 0 {
			first = msg
			if msg != want {
				t.Fatalf("message = %q, want %q", msg, want)
			}
			continue
		}
		if msg != first {
			t.Fatalf("message changed: %q vs %q", msg, first)
		}
	}
}

func TestUnknownKeyAlongsideLegalKeysPanics(t *testing.T) {
	err := mustPanic(t, func() {
		_, _ = Resolve(map[string]any{
			"Verbose":    true,
			"MaxRetries": 1,
			"LogLevel":   "info",
			"TimeoutSec": 1,
			"extra":      true,
		}, nil, nil, nil)
	})
	if !strings.Contains(err.Error(), `"extra"`) {
		t.Fatalf("message %q missing unknown key", err)
	}
}

func TestUnknownKeyListCapped(t *testing.T) {
	// Multibyte runes sort after ASCII, so every other key must sort after
	// the overlong key for truncation to appear in the shown window.
	long := strings.Repeat("世", unknownKeyRuneLimit+1)
	keys := map[string]any{
		long: 1,
		"啊":  1,
		"哦":  1,
		"嗯":  1,
		"呀":  1,
		"哇":  1,
		"哈":  1,
		"呐":  1,
	}
	err := mustPanic(t, func() {
		_, _ = Resolve(nil, keys, nil, nil)
	})
	msg := err.Error()
	wantPrefix := `config: layer "file": unknown key(s) `
	if !strings.HasPrefix(msg, wantPrefix) {
		t.Fatalf("message %q missing prefix", msg)
	}
	if !strings.Contains(msg, " and 3 more") {
		t.Fatalf("message %q missing remainder count", msg)
	}
	shown := strings.TrimPrefix(msg, wantPrefix)
	shown = strings.TrimSuffix(shown, " and 3 more")
	parts := strings.Split(shown, ", ")
	if len(parts) != unknownKeyLimit {
		t.Fatalf("shown keys = %d, want %d (%q)", len(parts), unknownKeyLimit, msg)
	}
	truncated := strconv.Quote(string([]rune(long)[:unknownKeyRuneLimit]) + "...")
	if !strings.Contains(msg, truncated) {
		t.Fatalf("message %q missing truncated key %s", msg, truncated)
	}
	if strings.Contains(msg, long) {
		t.Fatalf("message includes the full overlong key")
	}
}

func TestFieldsCoverConfig(t *testing.T) {
	typ := reflect.TypeOf(Config{})
	keys := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		keys[f.key] = struct{}{}
	}
	exported := 0
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.PkgPath != "" {
			continue
		}
		exported++
		if _, ok := keys[f.Name]; !ok {
			t.Errorf("exported field %s missing from fields table", f.Name)
		}
	}
	if len(fields) != exported {
		t.Fatalf("len(fields)=%d, exported fields=%d", len(fields), exported)
	}
}

func TestEachKeyWritesOnlyItsField(t *testing.T) {
	typ := reflect.TypeOf(Config{})
	for _, f := range fields {
		t.Run(f.key, func(t *testing.T) {
			sf, ok := typ.FieldByName(f.key)
			if !ok {
				t.Fatalf("Config has no field %s", f.key)
			}
			presented := sampleValue(sf.Type)
			got, err := Resolve(nil, nil, nil, map[string]any{f.key: presented})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			gotv := reflect.ValueOf(got)
			for i := 0; i < typ.NumField(); i++ {
				field := typ.Field(i)
				gotField := gotv.Field(i)
				if field.Name == f.key {
					if !reflect.DeepEqual(gotField.Interface(), presented) {
						t.Fatalf("field %s = %#v, want %#v", field.Name, gotField.Interface(), presented)
					}
					continue
				}
				zero := reflect.Zero(field.Type).Interface()
				if !reflect.DeepEqual(gotField.Interface(), zero) {
					t.Fatalf("key %s also wrote %s = %#v", f.key, field.Name, gotField.Interface())
				}
			}
		})
	}
}

func sampleValue(typ reflect.Type) any {
	switch typ.Kind() {
	case reflect.Bool:
		return true
	case reflect.Int:
		return 11
	case reflect.String:
		return "info"
	default:
		panic("sampleValue: unsupported Config field type " + typ.String())
	}
}

func TestDuplicateFieldRegistration(t *testing.T) {
	err := mustPanic(t, func() {
		indexFields([]field{
			{key: "Verbose"},
			{key: "MaxRetries"},
			{key: "Verbose"},
		})
	})
	if !strings.Contains(err.Error(), "Verbose") {
		t.Fatalf("panic %q does not name the duplicated key", err)
	}
}

func layerSlot(slot int, values map[string]any) (map[string]any, map[string]any, map[string]any, map[string]any) {
	var layers [4]map[string]any
	layers[slot] = values
	return layers[0], layers[1], layers[2], layers[3]
}

func mustPanic(t *testing.T, fn func()) error {
	t.Helper()
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		fn()
	}()
	if recovered == nil {
		t.Fatal("expected panic")
	}
	err, ok := recovered.(error)
	if !ok {
		t.Fatalf("panic value type %T, want error: %v", recovered, recovered)
	}
	return err
}
