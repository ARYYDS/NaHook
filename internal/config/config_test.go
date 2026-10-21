package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func validConfig() *Config {
	return &Config{
		Listen:        "127.0.0.1:19080",
		ControlSocket: "/run/nahook/ctl.sock",
		NewAPI:        "http://127.0.0.1:3000",
		BodyLimit:     64 << 20,
		Match: Match{
			Paths:  []string{"/v1/responses", "/v1/chat/completions"},
			Models: []string{"gpt-6-astra"},
		},
		Retry: Retry{Max: 3, PerModel: map[string]int{}},
		Gate:  GateConf{BaseFloorMS: 50, StreakCap: 3},
	}
}

// specSample is the configuration from design §5, verbatim.
const specSample = `{
  "listen": "127.0.0.1:19080",
  "control_socket": "/run/nahook/ctl.sock",
  "newapi": "http://127.0.0.1:3000",
  "body_limit": 67108864,
  "match": { "paths": ["/v1/responses", "/v1/chat/completions"], "models": ["gpt-6-astra"] },
  "retry": { "max": 3, "per_model": {} },
  "gate":  { "base_floor_ms": 50, "streak_cap": 3 }
}`

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr error
	}{
		{name: "valid", mutate: func(*Config) {}},
		{name: "nil per_model", mutate: func(c *Config) { c.Retry.PerModel = nil }},
		{name: "empty models selects all streaming", mutate: func(c *Config) { c.Match.Models = nil }},
		{name: "empty paths intercepts nothing", mutate: func(c *Config) { c.Match.Paths = nil }},
		{name: "retry max zero is allowed", mutate: func(c *Config) { c.Retry.Max = 0 }},
		{name: "base floor zero is allowed", mutate: func(c *Config) { c.Gate.BaseFloorMS = 0 }},
		{name: "empty listen", mutate: func(c *Config) { c.Listen = "" }, wantErr: ErrInvalidConfig},
		{name: "empty control socket", mutate: func(c *Config) { c.ControlSocket = "" }, wantErr: ErrInvalidConfig},
		{name: "newapi without scheme", mutate: func(c *Config) { c.NewAPI = "127.0.0.1:3000" }, wantErr: ErrInvalidConfig},
		{name: "newapi wrong scheme", mutate: func(c *Config) { c.NewAPI = "ftp://127.0.0.1" }, wantErr: ErrInvalidConfig},
		{name: "newapi empty", mutate: func(c *Config) { c.NewAPI = "" }, wantErr: ErrInvalidConfig},
		{name: "body limit zero", mutate: func(c *Config) { c.BodyLimit = 0 }, wantErr: ErrInvalidConfig},
		{name: "body limit negative", mutate: func(c *Config) { c.BodyLimit = -1 }, wantErr: ErrInvalidConfig},
		{name: "negative base floor", mutate: func(c *Config) { c.Gate.BaseFloorMS = -1 }, wantErr: ErrInvalidConfig},
		{name: "streak cap zero", mutate: func(c *Config) { c.Gate.StreakCap = 0 }, wantErr: ErrInvalidConfig},
		{name: "streak cap at the overflow bound", mutate: func(c *Config) { c.Gate.StreakCap = maxStreakCap }},
		{name: "streak cap past the overflow bound", mutate: func(c *Config) { c.Gate.StreakCap = maxStreakCap + 1 }, wantErr: ErrInvalidConfig},
		{name: "per-model empty key", mutate: func(c *Config) { c.Retry.PerModel[""] = 1 }, wantErr: ErrInvalidRetry},
		{name: "per-model global key", mutate: func(c *Config) { c.Retry.PerModel["*"] = 1 }, wantErr: ErrInvalidRetry},
		{name: "negative retry max", mutate: func(c *Config) { c.Retry.Max = -1 }, wantErr: ErrInvalidRetry},
		{
			name:    "negative per-model retry max",
			mutate:  func(c *Config) { c.Retry.PerModel["gpt-6-lunna"] = -2 },
			wantErr: ErrInvalidRetry,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			tt.mutate(c)
			err := c.Validate()
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestSpecSampleConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(specSample), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("design §5 sample must validate: %v", err)
	}
	if c.Listen != "127.0.0.1:19080" || c.ControlSocket != "/run/nahook/ctl.sock" {
		t.Fatalf("listen/control_socket = %q/%q", c.Listen, c.ControlSocket)
	}
	if c.NewAPI != "http://127.0.0.1:3000" || c.BodyLimit != 64<<20 {
		t.Fatalf("newapi/body_limit = %q/%d", c.NewAPI, c.BodyLimit)
	}
	if c.Retry.Max != 3 || len(c.Retry.PerModel) != 0 {
		t.Fatalf("retry = %+v", c.Retry)
	}
	if c.Gate.BaseFloorMS != 50 || c.Gate.StreakCap != 3 {
		t.Fatalf("gate = %+v", c.Gate)
	}
	if !reflect.DeepEqual(c.Match.Paths, []string{"/v1/responses", "/v1/chat/completions"}) {
		t.Fatalf("paths = %v", c.Match.Paths)
	}
	if !reflect.DeepEqual(c.Match.Models, []string{"gpt-6-astra"}) {
		t.Fatalf("models = %v", c.Match.Models)
	}
}

func TestPersistLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	want := validConfig()
	if err := Persist(path, want); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("persisted config must validate: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.HasSuffix(string(b), "}\n") {
		t.Fatalf("file must end with a newline, got %q", tail(string(b), 8))
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "config.json" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("temporary file left behind: %v", names)
	}
}

func TestLoadParsesAndDoesNotValidate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"listen":"","retry":{"max":-4}}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load must not reject on content, got %v", err)
	}
	if err := c.Validate(); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Validate() = %v, want ErrInvalidConfig", err)
	}
}

func TestLoadErrors(t *testing.T) {
	dir := t.TempDir()
	broken := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(broken, []byte("{ not json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := Load(broken); err == nil {
		t.Fatal("Load(broken JSON) = nil, want error")
	}
	if _, err := Load(filepath.Join(dir, "absent.json")); !os.IsNotExist(err) {
		t.Fatalf("Load(absent) = %v, want not-exist", err)
	}
}

func TestPersistReportsFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent-dir", "config.json")
	if err := Persist(path, validConfig()); err == nil {
		t.Fatal("Persist into a missing directory = nil, want error")
	}
}

// A misspelt key must not silently fall back to a zero value (design §6.4.1).
func TestLoadRejectsUnknownKeysAndTrailingData(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "misspelt nested key", body: `{"retry":{"maxx":3}}`},
		{name: "misspelt top-level key", body: `{"lisen":"127.0.0.1:19080"}`},
		{name: "second object after the configuration", body: specSample + `{}`},
		{name: "trailing garbage", body: specSample + ` x`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			if c, err := Load(path); err == nil {
				t.Fatalf("Load = %+v, nil; want an error", c)
			}
		})
	}
}

// Empty collections are written as {} and [] like the design §5 sample.
func TestPersistWritesEmptyCollections(t *testing.T) {
	c := validConfig()
	c.Match.Paths, c.Match.Models, c.Retry.PerModel = nil, nil, nil
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Persist(path, c); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	for _, want := range []string{`"paths": []`, `"models": []`, `"per_model": {}`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("persisted file lacks %s:\n%s", want, b)
		}
	}
	if c.Match.Paths != nil || c.Retry.PerModel != nil {
		t.Fatal("Persist must not modify the config it writes")
	}
}

// failDirSync makes the directory fsync step fail for the rest of the test.
func failDirSync(t *testing.T) {
	t.Helper()
	orig := syncDirFn
	syncDirFn = func(string) error { return errors.New("injected EIO") }
	t.Cleanup(func() { syncDirFn = orig })
}

// After the rename the new content is in place even when the directory fsync
// fails; Persist reports that as ErrDirSync (design §6.4.1).
func TestPersistDirSyncFailureReportsErrDirSync(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	failDirSync(t)
	c := validConfig()
	c.Retry.Max = 7
	if err := Persist(path, c); !errors.Is(err, ErrDirSync) {
		t.Fatalf("Persist = %v, want ErrDirSync", err)
	}
	if got := mustLoad(t, path); got.Retry.Max != 7 {
		t.Fatalf("file retry.max = %d, want 7 (the rename happened)", got.Retry.Max)
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
