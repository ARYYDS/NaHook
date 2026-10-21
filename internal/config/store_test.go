package config

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// newTestStore persists c and returns a store bound to that file, so every
// write path exercises the real persist step.
func newTestStore(t *testing.T, c *Config) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Persist(path, c); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	return NewStore(path, c), path
}

func mustLoad(t *testing.T, path string) *Config {
	t.Helper()
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c
}

func TestRetryMaxLookup(t *testing.T) {
	base := *validConfig()
	tests := []struct {
		name      string
		perModel  map[string]int
		globalMax int
		model     string
		want      int
	}{
		{name: "global only", perModel: map[string]int{}, globalMax: 3, model: "gpt-6-astra", want: 3},
		{name: "nil per_model", perModel: nil, globalMax: 5, model: "gpt-6-lunna", want: 5},
		{name: "override wins", perModel: map[string]int{"gpt-6-astra": 1}, globalMax: 3, model: "gpt-6-astra", want: 1},
		{name: "other model keeps global", perModel: map[string]int{"gpt-6-astra": 1}, globalMax: 3, model: "gpt-6-lunna", want: 3},
		{name: "override may be zero", perModel: map[string]int{"gpt-6-astra": 0}, globalMax: 3, model: "gpt-6-astra", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base
			c.Match = Match{Paths: append([]string(nil), base.Match.Paths...), Models: append([]string(nil), base.Match.Models...)}
			c.Retry = Retry{Max: tt.globalMax, PerModel: tt.perModel}
			s := NewStore(filepath.Join(t.TempDir(), "config.json"), &c)
			if got := s.RetryMax(tt.model); got != tt.want {
				t.Fatalf("RetryMax(%q) = %d, want %d", tt.model, got, tt.want)
			}
		})
	}
}

func TestSetRetryMaxRejectsNegative(t *testing.T) {
	s, path := newTestStore(t, validConfig())
	before := s.Current()
	fileBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	for _, scope := range []string{"*", "gpt-6-astra"} {
		old, err := s.SetRetryMax(scope, -1)
		if !errors.Is(err, ErrInvalidRetry) {
			t.Fatalf("SetRetryMax(%q, -1) error = %v, want ErrInvalidRetry", scope, err)
		}
		if old != 0 {
			t.Fatalf("SetRetryMax(%q, -1) old = %d, want 0", scope, old)
		}
	}

	if s.Current() != before {
		t.Fatal("snapshot was replaced by a rejected write")
	}
	if got := s.RetryMax("gpt-6-astra"); got != 3 {
		t.Fatalf("RetryMax after rejected write = %d, want 3", got)
	}
	fileAfter, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(fileAfter) != string(fileBefore) {
		t.Fatalf("file changed by a rejected write:\n before %.200s\n after  %.200s", fileBefore, fileAfter)
	}
}

func TestSetRetryMaxGlobalAndPerModel(t *testing.T) {
	s, path := newTestStore(t, validConfig())

	old, err := s.SetRetryMax("*", 5)
	if err != nil {
		t.Fatalf("SetRetryMax(*, 5): %v", err)
	}
	if old != 3 {
		t.Fatalf("old global = %d, want 3", old)
	}
	if got := s.RetryMax("anything"); got != 5 {
		t.Fatalf("RetryMax after global write = %d, want 5", got)
	}
	if c := mustLoad(t, path); c.Retry.Max != 5 {
		t.Fatalf("file global max = %d, want 5", c.Retry.Max)
	}

	old, err = s.SetRetryMax("gpt-6-astra", 2)
	if err != nil {
		t.Fatalf("SetRetryMax(model, 2): %v", err)
	}
	if old != 5 {
		t.Fatalf("old for model = %d, want 5 (effective global value)", old)
	}
	if got := s.RetryMax("gpt-6-astra"); got != 2 {
		t.Fatalf("RetryMax(model) = %d, want 2", got)
	}
	if got := s.RetryMax("gpt-6-lunna"); got != 5 {
		t.Fatalf("RetryMax(other) = %d, want 5", got)
	}
	c := mustLoad(t, path)
	if c.Retry.Max != 5 || c.Retry.PerModel["gpt-6-astra"] != 2 {
		t.Fatalf("file = %+v, want max 5 and per_model 2", c.Retry)
	}
}

func TestSetRetryMaxDoesNotMutateOldSnapshot(t *testing.T) {
	s, _ := newTestStore(t, validConfig())
	old := s.Current()

	if _, err := s.SetRetryMax("gpt-6-astra", 7); err != nil {
		t.Fatalf("SetRetryMax: %v", err)
	}
	if _, ok := old.Retry.PerModel["gpt-6-astra"]; ok {
		t.Fatal("published snapshot gained a per-model entry")
	}
	if old.Retry.Max != 3 {
		t.Fatalf("published snapshot global max = %d, want 3", old.Retry.Max)
	}
}

func TestSetRetryMaxPersistFailureKeepsSnapshot(t *testing.T) {
	c := validConfig()
	s := NewStore(filepath.Join(t.TempDir(), "absent-dir", "config.json"), c)
	before := s.Current()

	if _, err := s.SetRetryMax("*", 9); err == nil {
		t.Fatal("SetRetryMax against an unwritable path = nil, want error")
	}
	if s.Current() != before {
		t.Fatal("snapshot changed although the write failed")
	}
	if got := s.RetryMax("gpt-6-astra"); got != 3 {
		t.Fatalf("RetryMax after failed write = %d, want 3", got)
	}
}

func TestUnsetRetryMax(t *testing.T) {
	s, path := newTestStore(t, validConfig())
	if _, err := s.SetRetryMax("gpt-6-astra", 1); err != nil {
		t.Fatalf("SetRetryMax: %v", err)
	}
	if err := s.UnsetRetryMax("gpt-6-astra"); err != nil {
		t.Fatalf("UnsetRetryMax: %v", err)
	}
	if got := s.RetryMax("gpt-6-astra"); got != 3 {
		t.Fatalf("RetryMax after unset = %d, want the global 3", got)
	}
	if c := mustLoad(t, path); len(c.Retry.PerModel) != 0 {
		t.Fatalf("file still has per_model %+v", c.Retry.PerModel)
	}
	if err := s.UnsetRetryMax("gpt-6-astra"); !errors.Is(err, ErrRetryNotSet) {
		t.Fatalf("second unset = %v, want ErrRetryNotSet", err)
	}
	if err := s.UnsetRetryMax("never-configured"); !errors.Is(err, ErrRetryNotSet) {
		t.Fatalf("unset of an unknown model = %v, want ErrRetryNotSet", err)
	}
}

func TestReloadFailureKeepsOldSnapshot(t *testing.T) {
	s, path := newTestStore(t, validConfig())
	before := s.Current()

	if err := os.WriteFile(path, []byte("{ broken"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := s.Reload(); err == nil {
		t.Fatal("Reload of broken JSON = nil, want error")
	}
	if s.Current() != before {
		t.Fatal("snapshot was replaced by a failed reload")
	}

	bad := validConfig()
	bad.Retry.Max = -1
	if err := Persist(path, bad); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if err := s.Reload(); !errors.Is(err, ErrInvalidRetry) {
		t.Fatalf("Reload of an invalid config = %v, want ErrInvalidRetry", err)
	}
	if s.Current() != before {
		t.Fatal("snapshot was replaced by an invalid reload")
	}
	if got := s.RetryMax("gpt-6-astra"); got != 3 {
		t.Fatalf("RetryMax after failed reload = %d, want 3", got)
	}
}

func TestReloadPublishesNewFile(t *testing.T) {
	s, path := newTestStore(t, validConfig())
	before := s.Current()

	next := validConfig()
	next.Listen = "127.0.0.1:19181"
	next.Retry.Max = 0
	next.Match.Models = nil
	if err := Persist(path, next); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if s.Current() == before {
		t.Fatal("Reload did not publish a new snapshot")
	}
	if got := s.Current().Listen; got != "127.0.0.1:19181" {
		t.Fatalf("listen = %q, want the reloaded value", got)
	}
	if got := s.RetryMax("gpt-6-astra"); got != 0 {
		t.Fatalf("RetryMax = %d, want the reloaded 0", got)
	}
}

// TestConcurrentReadersAndWriters is the -race probe: the data plane only reads
// the snapshot while the control plane keeps replacing it.
func TestConcurrentReadersAndWriters(t *testing.T) {
	s, _ := newTestStore(t, validConfig())

	const (
		readers = 4
		writes  = 40
	)
	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				c := s.Current()
				if c.Listen == "" {
					t.Error("reader saw an empty listen address")
					return
				}
				_ = s.RetryMax("gpt-6-astra")
				_ = c.Retry.PerModel["gpt-6-lunna"]
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stop)
		for i := 0; i < writes; i++ {
			if _, err := s.SetRetryMax("gpt-6-astra", i%4); err != nil {
				t.Errorf("SetRetryMax: %v", err)
				return
			}
			if i%10 == 0 {
				if err := s.Reload(); err != nil {
					t.Errorf("Reload: %v", err)
					return
				}
			}
		}
	}()

	wg.Wait()
}
