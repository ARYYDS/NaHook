package config

import (
	"bytes"
	"encoding/json"
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

// A store whose file does not exist (here the directory is missing too) refuses
// the write at the file check and keeps its snapshot: nahook never writes a
// configuration it could not read back (design §1.7).
func TestSetRetryMaxRefusesMissingFile(t *testing.T) {
	c := validConfig()
	s := NewStore(filepath.Join(t.TempDir(), "absent-dir", "config.json"), c)
	before := s.Current()

	if _, err := s.SetRetryMax("*", 9); !errors.Is(err, ErrFileChanged) {
		t.Fatalf("SetRetryMax against a missing file = %v, want ErrFileChanged", err)
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
	next.Retry.Max = 0
	next.Match.Models = nil
	next.Gate.StreakCap = 2
	if err := Persist(path, next); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if s.Current() == before {
		t.Fatal("Reload did not publish a new snapshot")
	}
	if got := s.RetryMax("gpt-6-astra"); got != 0 {
		t.Fatalf("RetryMax = %d, want the reloaded 0", got)
	}
	if got := s.Current().Gate.StreakCap; got != 2 {
		t.Fatalf("streak_cap = %d, want the reloaded 2", got)
	}
}

// listen, control_socket and newapi are bound at start-up; a reload that
// changes one of them is refused and the snapshot stays (design §1.7).
func TestReloadRejectsRestartOnlyFields(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "listen", mutate: func(c *Config) { c.Listen = "127.0.0.1:19181" }},
		{name: "control_socket", mutate: func(c *Config) { c.ControlSocket = "/run/nahook-test/ctl.sock" }},
		{name: "newapi", mutate: func(c *Config) { c.NewAPI = "http://127.0.0.1:3001" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, path := newTestStore(t, validConfig())
			before := s.Current()
			next := validConfig()
			tt.mutate(next)
			next.Retry.Max = 9
			if err := Persist(path, next); err != nil {
				t.Fatalf("Persist: %v", err)
			}
			if err := s.Reload(); !errors.Is(err, ErrRestartRequired) {
				t.Fatalf("Reload = %v, want ErrRestartRequired", err)
			}
			if s.Current() != before {
				t.Fatal("a refused reload must keep the previous snapshot")
			}
		})
	}
}

func TestSetRetryMaxRejectsEmptyScope(t *testing.T) {
	s, _ := newTestStore(t, validConfig())
	before := s.Current()
	if _, err := s.SetRetryMax("", 2); !errors.Is(err, ErrInvalidRetry) {
		t.Fatalf("SetRetryMax(\"\") = %v, want ErrInvalidRetry", err)
	}
	if s.Current() != before {
		t.Fatal("a rejected write must keep the previous snapshot")
	}
}

// When only the directory fsync fails, the file already holds the new value,
// so the snapshot must follow it and the caller learns about it through
// ErrDirSync (design §1.7).
func TestSetRetryMaxDirSyncFailureStillApplies(t *testing.T) {
	s, path := newTestStore(t, validConfig())
	failDirSync(t)
	old, err := s.SetRetryMax("gpt-6-astra", 6)
	if !errors.Is(err, ErrDirSync) {
		t.Fatalf("SetRetryMax = %v, want ErrDirSync", err)
	}
	if old != 3 {
		t.Fatalf("old = %d, want the effective value 3", old)
	}
	if got := s.RetryMax("gpt-6-astra"); got != 6 {
		t.Fatalf("snapshot RetryMax = %d, want 6", got)
	}
	if got := mustLoad(t, path).Retry.PerModel["gpt-6-astra"]; got != 6 {
		t.Fatalf("file per_model = %d, want 6", got)
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

// readFile is os.ReadFile with the test's error handling.
func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	return b
}

// writeFile replaces the configuration file behind the store's back, the way an
// operator editing it by hand would.
func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// TestSameConfig covers the content comparison behind checkFile (design §1.7):
// nil and an empty collection are the same configuration, and every field that
// can differ is detected.
func TestSameConfig(t *testing.T) {
	base := validConfig()
	if !sameConfig(base, clone(base)) {
		t.Fatal("a clone differs from its original")
	}

	empty := validConfig()
	empty.Match.Paths = []string{}
	empty.Match.Models = nil
	empty.Retry.PerModel = nil
	nilish := validConfig()
	nilish.Match.Paths = nil
	nilish.Match.Models = []string{}
	nilish.Retry.PerModel = map[string]int{}
	if !sameConfig(empty, nilish) {
		t.Fatal("nil and an empty collection must compare equal")
	}

	diffs := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "listen", mutate: func(c *Config) { c.Listen = "127.0.0.1:19181" }},
		{name: "control_socket", mutate: func(c *Config) { c.ControlSocket = "/run/other/ctl.sock" }},
		{name: "newapi", mutate: func(c *Config) { c.NewAPI = "http://127.0.0.1:3001" }},
		{name: "body_limit", mutate: func(c *Config) { c.BodyLimit++ }},
		{name: "paths", mutate: func(c *Config) { c.Match.Paths = append(c.Match.Paths, "/v1/other") }},
		{name: "models", mutate: func(c *Config) { c.Match.Models = append(c.Match.Models, "gpt-6-lunna") }},
		{name: "retry.max", mutate: func(c *Config) { c.Retry.Max++ }},
		{name: "per_model", mutate: func(c *Config) { c.Retry.PerModel["gpt-6-astra"] = 1 }},
		{name: "base_floor_ms", mutate: func(c *Config) { c.Gate.BaseFloorMS++ }},
		{name: "streak_cap", mutate: func(c *Config) { c.Gate.StreakCap++ }},
	}
	for _, tt := range diffs {
		other := validConfig()
		tt.mutate(other)
		if sameConfig(base, other) {
			t.Errorf("%s: sameConfig = true, want false", tt.name)
		}
	}
}

// TestSetRetryMaxRejectsChangedFile is the Q22 guard: a hand edit that was not
// reloaded must not be overwritten by a write built from the snapshot (design
// §1.7).
func TestSetRetryMaxRejectsChangedFile(t *testing.T) {
	s, path := newTestStore(t, validConfig())
	before := s.Current()

	edited := mustLoad(t, path)
	edited.Match.Models = append(edited.Match.Models, "gpt-6-lunna")
	if err := Persist(path, edited); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	editedBytes := readFile(t, path)

	old, err := s.SetRetryMax("*", 9)
	if !errors.Is(err, ErrFileChanged) {
		t.Fatalf("SetRetryMax = %v, want ErrFileChanged", err)
	}
	if old != 0 {
		t.Fatalf("old = %d, want 0 on a refused write", old)
	}
	if s.Current() != before {
		t.Fatal("a refused write replaced the snapshot")
	}
	if got := readFile(t, path); !bytes.Equal(got, editedBytes) {
		t.Fatal("a refused write changed the file")
	}
	if got := s.RetryMax("gpt-6-astra"); got != 3 {
		t.Fatalf("RetryMax = %d, want the snapshot value 3", got)
	}
}

// TestSetRetryMaxAcceptsCosmeticDifference: a file that only differs in layout,
// or whose empty collections came back as [] and {}, still holds the snapshot and
// is written back as usual (design §1.7).
func TestSetRetryMaxAcceptsCosmeticDifference(t *testing.T) {
	// Persist writes [] and {} while the snapshot keeps nil, which is exactly the
	// state every start-up leaves behind.
	c := validConfig()
	c.Match.Paths = nil
	c.Match.Models = nil
	c.Retry.PerModel = nil
	s, path := newTestStore(t, c)

	if _, err := s.SetRetryMax("gpt-6-astra", 2); err != nil {
		t.Fatalf("SetRetryMax right after Persist = %v, want success", err)
	}
	if got := mustLoad(t, path).Retry.PerModel["gpt-6-astra"]; got != 2 {
		t.Fatalf("file per_model = %d, want 2", got)
	}

	// Re-encoding the same content without the indentation changes nothing.
	compact, err := json.Marshal(mustLoad(t, path))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	writeFile(t, path, compact)
	if _, err := s.SetRetryMax("*", 4); err != nil {
		t.Fatalf("SetRetryMax after a cosmetic rewrite = %v, want success", err)
	}
	if got := s.RetryMax("anything"); got != 4 {
		t.Fatalf("RetryMax = %d, want 4", got)
	}
}

// TestSetRetryMaxRejectsUnreadableFile: a file this process cannot parse or
// cannot read at all is refused instead of overwritten (design §1.7).
func TestSetRetryMaxRejectsUnreadableFile(t *testing.T) {
	t.Run("not valid JSON", func(t *testing.T) {
		s, path := newTestStore(t, validConfig())
		before := s.Current()
		writeFile(t, path, []byte("{ broken"))

		if _, err := s.SetRetryMax("*", 5); !errors.Is(err, ErrFileChanged) {
			t.Fatalf("SetRetryMax = %v, want ErrFileChanged", err)
		}
		if s.Current() != before {
			t.Fatal("a refused write replaced the snapshot")
		}
		if got := readFile(t, path); string(got) != "{ broken" {
			t.Fatalf("a refused write changed the file: %q", got)
		}
	})

	t.Run("deleted", func(t *testing.T) {
		s, path := newTestStore(t, validConfig())
		before := s.Current()
		if err := os.Remove(path); err != nil {
			t.Fatalf("Remove: %v", err)
		}

		if _, err := s.SetRetryMax("*", 5); !errors.Is(err, ErrFileChanged) {
			t.Fatalf("SetRetryMax = %v, want ErrFileChanged", err)
		}
		if s.Current() != before {
			t.Fatal("a refused write replaced the snapshot")
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("a refused write created the file again")
		}
	})
}

// TestUnsetRetryMaxRejectsChangedFile is the same guard on the delete path.
func TestUnsetRetryMaxRejectsChangedFile(t *testing.T) {
	s, path := newTestStore(t, validConfig())
	if _, err := s.SetRetryMax("gpt-6-astra", 1); err != nil {
		t.Fatalf("SetRetryMax: %v", err)
	}

	edited := mustLoad(t, path)
	edited.Match.Models = append(edited.Match.Models, "gpt-6-lunna")
	if err := Persist(path, edited); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	editedBytes := readFile(t, path)
	before := s.Current()

	if err := s.UnsetRetryMax("gpt-6-astra"); !errors.Is(err, ErrFileChanged) {
		t.Fatalf("UnsetRetryMax = %v, want ErrFileChanged", err)
	}
	if s.Current() != before {
		t.Fatal("a refused unset replaced the snapshot")
	}
	if got := readFile(t, path); !bytes.Equal(got, editedBytes) {
		t.Fatal("a refused unset changed the file")
	}
	if got := s.RetryMax("gpt-6-astra"); got != 1 {
		t.Fatalf("RetryMax = %d, want 1", got)
	}

	// A model without a per-model value has nothing to write, so the file is not
	// touched and the answer is ErrRetryNotSet even while the file differs.
	if err := s.UnsetRetryMax("gpt-6-lunna"); !errors.Is(err, ErrRetryNotSet) {
		t.Fatalf("UnsetRetryMax of an unset model = %v, want ErrRetryNotSet", err)
	}
	if got := readFile(t, path); !bytes.Equal(got, editedBytes) {
		t.Fatal("an ErrRetryNotSet answer changed the file")
	}
}

// TestFileStatus backs GET /v1/config: in sync after a write, changed after a
// hand edit, and an error when the file cannot be read (design §5).
func TestFileStatus(t *testing.T) {
	s, path := newTestStore(t, validConfig())
	if inSync, err := s.FileStatus(); err != nil || !inSync {
		t.Fatalf("FileStatus = %v, %v, want true and no error", inSync, err)
	}

	edited := mustLoad(t, path)
	edited.Retry.Max = 7
	if err := Persist(path, edited); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if inSync, err := s.FileStatus(); err != nil || inSync {
		t.Fatalf("FileStatus after a hand edit = %v, %v, want false and no error", inSync, err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if inSync, err := s.FileStatus(); err == nil || inSync {
		t.Fatalf("FileStatus of a missing file = %v, %v, want false and an error", inSync, err)
	}
}
