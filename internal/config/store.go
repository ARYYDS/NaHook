package config

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
)

// Store publishes configuration snapshots (interface 2.4). p is read by the
// data plane without a lock; wmu only serialises the control-plane writers so
// that the file and the snapshot stay consistent (design §4, §1.7).
type Store struct {
	p    atomic.Pointer[Config]
	wmu  sync.Mutex
	path string
}

// NewStore publishes c as the first snapshot and remembers the file that later
// changes are written to.
func NewStore(path string, c *Config) *Store {
	s := &Store{path: path}
	s.p.Store(c)
	return s
}

// Current returns the published snapshot. The caller must not modify it.
func (s *Store) Current() *Config {
	return s.p.Load()
}

// RetryMax is the retry ceiling in effect for model (design §1.7).
func (s *Store) RetryMax(model string) int {
	return retryMaxFor(s.p.Load(), model)
}

// SetRetryMax sets the ceiling for scope: "*" for the global value, otherwise a
// model name. old is the value that was in effect for scope before the change.
// The order is check the file → validate → persist → swap (design §1.7): a write
// that fails before the rename leaves both the file and the snapshot untouched.
// A file that was edited by hand and not reloaded is refused with
// ErrFileChanged, because next is built from the snapshot and would drop those
// edits. When only the directory fsync fails the new file is already in place,
// so the snapshot follows it and the ErrDirSync error is returned with old.
func (s *Store) SetRetryMax(scope string, n int) (old int, err error) {
	if n < 0 || scope == "" {
		return 0, ErrInvalidRetry
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()

	cur := s.p.Load()
	if err := s.checkFile(cur); err != nil {
		return 0, err
	}
	next := clone(cur)
	if scope == "*" {
		old = cur.Retry.Max
		next.Retry.Max = n
	} else {
		old = retryMaxFor(cur, scope)
		if next.Retry.PerModel == nil {
			next.Retry.PerModel = make(map[string]int, 1)
		}
		next.Retry.PerModel[scope] = n
	}
	if err := next.Validate(); err != nil {
		return 0, err
	}
	err = s.persist(next)
	if err != nil && !errors.Is(err, ErrDirSync) {
		return 0, err
	}
	return old, err
}

// UnsetRetryMax drops the per-model ceiling, so the model falls back to the
// global value. It reports ErrRetryNotSet when the model has none, and refuses
// with ErrFileChanged when the file was edited and not reloaded (design §1.7).
func (s *Store) UnsetRetryMax(model string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	cur := s.p.Load()
	if _, ok := cur.Retry.PerModel[model]; !ok {
		// Nothing to remove, so there is nothing to write and nothing to guard
		// against: the file is not touched either way.
		return ErrRetryNotSet
	}
	if err := s.checkFile(cur); err != nil {
		return err
	}
	next := clone(cur)
	delete(next.Retry.PerModel, model)
	if err := next.Validate(); err != nil {
		return err
	}
	return s.persist(next)
}

// persist writes next and publishes it once the file holds it: after a
// successful write, and also when only the directory fsync failed (ErrDirSync),
// so the file and the snapshot never disagree. The caller holds wmu.
func (s *Store) persist(next *Config) error {
	err := Persist(s.path, next)
	if err == nil || errors.Is(err, ErrDirSync) {
		s.p.Store(next)
	}
	return err
}

// checkFile reports whether the file still holds the configuration this store
// published. The caller holds wmu, so a concurrent write cannot slip in between
// the comparison and the rename that follows it. A file that cannot be read or
// parsed is refused as well: overwriting something this process cannot
// understand would lose whatever the operator meant by it (design §1.7).
//
// The comparison is by content, not by bytes: a file that only differs in
// indentation or key order still holds the same configuration and is written
// back as usual.
func (s *Store) checkFile(cur *Config) error {
	disk, err := Load(s.path)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrFileChanged, err)
	}
	if !sameConfig(disk, cur) {
		return ErrFileChanged
	}
	return nil
}

// sameConfig compares two configurations by content. nil and an empty
// collection are the same configuration: Persist writes an empty collection as
// [] or {}, Load reads it back as a non-nil empty value, and clone keeps a nil
// PerModel nil, so a byte or hash comparison would call a file that this store
// has just written "changed" (design §1.7).
func sameConfig(a, b *Config) bool {
	return a.Listen == b.Listen &&
		a.ControlSocket == b.ControlSocket &&
		a.NewAPI == b.NewAPI &&
		a.BodyLimit == b.BodyLimit &&
		a.Gate == b.Gate &&
		a.Retry.Max == b.Retry.Max &&
		slices.Equal(a.Match.Paths, b.Match.Paths) &&
		slices.Equal(a.Match.Models, b.Match.Models) &&
		maps.Equal(a.Retry.PerModel, b.Retry.PerModel)
}

// FileStatus reports whether the file on disk still matches the published
// snapshot; GET /v1/config shows it as "in_sync" or "changed" (design §5). The
// write lock keeps it from interleaving with a write in progress.
func (s *Store) FileStatus() (inSync bool, err error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	disk, err := Load(s.path)
	if err != nil {
		return false, err
	}
	return sameConfig(disk, s.p.Load()), nil
}

// Reload re-reads the file, validates it and publishes it. On any failure the
// previous snapshot stays in place (interface 2.4). The read happens under the
// write lock so a concurrent SetRetryMax cannot be undone by a stale reload.
// listen, control_socket and newapi are bound when the process starts, so a
// file that changes any of them is refused with ErrRestartRequired (design
// §1.7).
func (s *Store) Reload() error {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	next, err := Load(s.path)
	if err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	cur := s.p.Load()
	switch {
	case next.Listen != cur.Listen:
		return fmt.Errorf("%w: listen %q -> %q", ErrRestartRequired, cur.Listen, next.Listen)
	case next.ControlSocket != cur.ControlSocket:
		return fmt.Errorf("%w: control_socket %q -> %q", ErrRestartRequired, cur.ControlSocket, next.ControlSocket)
	case next.NewAPI != cur.NewAPI:
		return fmt.Errorf("%w: newapi %q -> %q", ErrRestartRequired, cur.NewAPI, next.NewAPI)
	}
	s.p.Store(next)
	return nil
}

func retryMaxFor(c *Config, model string) int {
	if v, ok := c.Retry.PerModel[model]; ok {
		return v
	}
	return c.Retry.Max
}

// clone deep-copies the parts of Config that a writer mutates, so a published
// snapshot never changes under a data-plane reader.
func clone(c *Config) *Config {
	next := *c
	next.Match.Paths = slices.Clone(c.Match.Paths)
	next.Match.Models = slices.Clone(c.Match.Models)
	if c.Retry.PerModel != nil {
		next.Retry.PerModel = make(map[string]int, len(c.Retry.PerModel))
		for model, n := range c.Retry.PerModel {
			next.Retry.PerModel[model] = n
		}
	}
	return &next
}
