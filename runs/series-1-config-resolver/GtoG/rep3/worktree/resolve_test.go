package config

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestAllFieldsAbsent(t *testing.T) {
	got, err := Resolve(nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != (Config{}) {
		t.Fatalf("got %+v, want zero Config", got)
	}
}

func TestIndependentFields(t *testing.T) {
	got, err := Resolve(
		map[string]any{"Verbose": false, "MaxRetries": 3, "LogLevel": "info", "TimeoutSec": 9},
		nil, nil,
		map[string]any{"Verbose": true},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{Verbose: true, MaxRetries: 3, LogLevel: "info", TimeoutSec: 9}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestMidLayerZeroDoesNotBlockHigher(t *testing.T) {
	got, err := Resolve(
		map[string]any{"MaxRetries": 3, "TimeoutSec": 9},
		map[string]any{"MaxRetries": 0},
		nil,
		map[string]any{"TimeoutSec": 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxRetries != 0 {
		t.Fatalf("MaxRetries = %d, want 0", got.MaxRetries)
	}
	if got.TimeoutSec != 1 {
		t.Fatalf("TimeoutSec = %d, want 1", got.TimeoutSec)
	}
}

func TestHighestLayerWinsVerbose(t *testing.T) {
	got, err := Resolve(
		map[string]any{"Verbose": true},
		map[string]any{"Verbose": false},
		map[string]any{"Verbose": true},
		map[string]any{"Verbose": false},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.Verbose != false {
		t.Fatalf("Verbose = %v, want false", got.Verbose)
	}
}

func TestOverriddenIllegalLevelNotValidated(t *testing.T) {
	got, err := Resolve(
		map[string]any{"LogLevel": "trace"},
		nil, nil,
		map[string]any{"LogLevel": "info"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.LogLevel != "info" {
		t.Fatalf("LogLevel = %q, want %q", got.LogLevel, "info")
	}

	got, err = Resolve(
		map[string]any{"LogLevel": "nope"},
		map[string]any{"LogLevel": ""},
		nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.LogLevel != "" {
		t.Fatalf("LogLevel = %q, want empty", got.LogLevel)
	}
}

func TestLegalAndIllegalLogLevels(t *testing.T) {
	for _, level := range []string{"", "debug", "info", "warn", "error"} {
		got, err := Resolve(nil, nil, nil, map[string]any{"LogLevel": level})
		if err != nil {
			t.Fatalf("level %q: unexpected error: %v", level, err)
		}
		if got.LogLevel != level {
			t.Fatalf("LogLevel = %q, want %q", got.LogLevel, level)
		}
	}
	for _, level := range []string{"trace", "Info", "DEBUG", " fatal", "nope"} {
		_, err := Resolve(nil, nil, nil, map[string]any{"LogLevel": level})
		if !errors.Is(err, ErrInvalidLogLevel) {
			t.Fatalf("level %q: err = %v, want ErrInvalidLogLevel", level, err)
		}
	}
}

func TestWrongTypePanics(t *testing.T) {
	type myInt int

	cases := []struct {
		name    string
		layers  [4]map[string]any
		wantKey string
	}{
		{
			name: "int64 overridden by higher int",
			layers: [4]map[string]any{
				{"MaxRetries": int64(1)},
				nil, nil,
				{"MaxRetries": 1},
			},
			wantKey: "MaxRetries",
		},
		{
			name: "float64",
			layers: [4]map[string]any{
				{"MaxRetries": float64(1)},
				nil, nil, nil,
			},
			wantKey: "MaxRetries",
		},
		{
			name: "Verbose string",
			layers: [4]map[string]any{
				{"Verbose": "yes"},
				nil, nil, nil,
			},
			wantKey: "Verbose",
		},
		{
			name: "LogLevel int",
			layers: [4]map[string]any{
				nil,
				{"LogLevel": 1},
				nil, nil,
			},
			wantKey: "LogLevel",
		},
		{
			name: "untyped nil",
			layers: [4]map[string]any{
				{"TimeoutSec": any(nil)},
				nil, nil, nil,
			},
			wantKey: "TimeoutSec",
		},
		{
			name: "named int",
			layers: [4]map[string]any{
				{"MaxRetries": myInt(3)},
				nil, nil, nil,
			},
			wantKey: "MaxRetries",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := recoverPanic(func() {
				_, _ = Resolve(tc.layers[0], tc.layers[1], tc.layers[2], tc.layers[3])
			})
			if msg == "" {
				t.Fatal("expected panic")
			}
			if !contains(msg, tc.wantKey) {
				t.Fatalf("panic %q does not contain key %q", msg, tc.wantKey)
			}
		})
	}
}

func TestUnknownKeysIgnored(t *testing.T) {
	got, err := Resolve(
		map[string]any{"Extra": 1, "verbose": true, "Verbose": true},
		nil, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.Verbose != true {
		t.Fatalf("Verbose = %v, want true", got.Verbose)
	}
	if got != (Config{Verbose: true}) {
		t.Fatalf("got %+v, want only Verbose", got)
	}
}

func TestRegistryBinding(t *testing.T) {
	type row struct {
		key string
		dst any
		val any
	}
	rows := []row{
		{"Verbose", func(c *Config) *bool { return &c.Verbose }, true},
		{"MaxRetries", func(c *Config) *int { return &c.MaxRetries }, 4},
		{"LogLevel", func(c *Config) *string { return &c.LogLevel }, "debug"},
		{"TimeoutSec", func(c *Config) *int { return &c.TimeoutSec }, 12},
	}
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.key] = true
		f := constructField(r.key, r.dst)
		var cfg Config
		f.apply(&cfg, r.val)
		assertOnlyField(t, cfg, r.key)

		prod, ok := registryField(r.key)
		if !ok {
			t.Fatalf("registry has no row for %s", r.key)
		}
		var fromRegistry Config
		prod.apply(&fromRegistry, r.val)
		assertOnlyField(t, fromRegistry, r.key)
	}

	ct := reflect.TypeOf(Config{})
	for i := 0; i < ct.NumField(); i++ {
		name := ct.Field(i).Name
		if !seen[name] {
			t.Fatalf("exported field %s has no registry row", name)
		}
	}
	if len(fields) != ct.NumField() {
		t.Fatalf("registry has %d rows, Config has %d exported fields", len(fields), ct.NumField())
	}
}

func TestNegativeBinding(t *testing.T) {
	// The constructor was given a pointer at TimeoutSec under the name
	// MaxRetries. The check must see that pointer, so applying 7 changes
	// TimeoutSec and this case fails the "only MaxRetries" assertion.
	f := fieldOf("MaxRetries", func(c *Config) *int { return &c.TimeoutSec })
	var cfg Config
	f.apply(&cfg, 7)
	if err := onlyFieldError(cfg, "MaxRetries"); err == nil {
		t.Fatal("mis-aimed pointer was accepted as a MaxRetries binding")
	}
	if err := onlyFieldError(cfg, "TimeoutSec"); err != nil {
		t.Fatal(err)
	}
}

func TestNilAndEmptyMapAreAbsent(t *testing.T) {
	gotNil, err := Resolve(nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	gotEmpty, err := Resolve(
		map[string]any{},
		map[string]any{},
		map[string]any{},
		map[string]any{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if gotNil != gotEmpty || gotNil != (Config{}) {
		t.Fatalf("nil %+v, empty %+v, want zero", gotNil, gotEmpty)
	}
}

func TestTwoWrongTypesPanicOnRegistryOrder(t *testing.T) {
	msg := recoverPanic(func() {
		_, _ = Resolve(
			map[string]any{"TimeoutSec": "no", "Verbose": 1},
			nil, nil, nil,
		)
	})
	if msg == "" {
		t.Fatal("expected panic")
	}
	if !contains(msg, "Verbose") {
		t.Fatalf("panic %q does not contain Verbose", msg)
	}
	if contains(msg, "TimeoutSec") {
		t.Fatalf("panic %q names later bad key TimeoutSec", msg)
	}
}

func TestValidationErrorIsOnlyLogLevelSentinel(t *testing.T) {
	other := errors.New("other")
	_, err := Resolve(nil, nil, nil, map[string]any{"LogLevel": "trace"})
	if err == nil {
		t.Fatal("expected error")
	}
	if errors.Is(err, other) {
		t.Fatal("log level error matches unrelated sentinel")
	}
	if !errors.Is(err, ErrInvalidLogLevel) {
		t.Fatalf("err = %v, want ErrInvalidLogLevel", err)
	}

	got, err := Resolve(
		map[string]any{"MaxRetries": -1, "TimeoutSec": -5, "Verbose": false, "LogLevel": "warn"},
		nil, nil, nil,
	)
	if err != nil {
		t.Fatalf("unvalidated fields returned %v", err)
	}
	if got.MaxRetries != -1 || got.TimeoutSec != -5 {
		t.Fatalf("got %+v", got)
	}
}

func TestLogLevelErrorNotOrderDependent(t *testing.T) {
	rowErr := errors.New("row failed")
	err := mergeErrors(rowErr, checkLogLevel("trace"))
	if !errors.Is(err, ErrInvalidLogLevel) {
		t.Fatalf("err = %v, want ErrInvalidLogLevel", err)
	}
	if errors.Is(err, rowErr) {
		t.Fatal("row error stole the log level sentinel")
	}

	err = mergeErrors(rowErr, checkLogLevel("info"))
	if !errors.Is(err, rowErr) {
		t.Fatalf("err = %v, want row error", err)
	}
}

// mergeErrors mirrors the post-merge priority in resolve: the allow-list
// error wins over a row validator when LogLevel is illegal.
func mergeErrors(rowErr, levelErr error) error {
	if levelErr != nil {
		return levelErr
	}
	return rowErr
}

func registryField(key string) (field, bool) {
	for i := range fields {
		if fields[i].key == key {
			return fields[i], true
		}
	}
	return field{}, false
}

func constructField(key string, dst any) field {
	switch d := dst.(type) {
	case func(*Config) *bool:
		return fieldOf(key, d)
	case func(*Config) *int:
		return fieldOf(key, d)
	case func(*Config) *string:
		return fieldOf(key, d)
	default:
		panic("unsupported pointer function")
	}
}

func assertOnlyField(t *testing.T, cfg Config, key string) {
	t.Helper()
	if err := onlyFieldError(cfg, key); err != nil {
		t.Fatal(err)
	}
}

func onlyFieldError(cfg Config, key string) error {
	v := reflect.ValueOf(cfg)
	changed := false
	for i := 0; i < v.NumField(); i++ {
		name := v.Type().Field(i).Name
		if v.Field(i).IsZero() {
			continue
		}
		changed = true
		if name != key {
			return errors.New("field " + name + " changed; want only " + key)
		}
	}
	if !changed {
		return errors.New("no field changed; want " + key)
	}
	return nil
}

func recoverPanic(fn func()) (msg string) {
	defer func() {
		if r := recover(); r != nil {
			s, ok := r.(string)
			if !ok {
				msg = ""
				return
			}
			msg = s
		}
	}()
	fn()
	return ""
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}
