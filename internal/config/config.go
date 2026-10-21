// Package config loads, validates, persists and publishes the nahook
// configuration (design §1.7, §5; interface 2.4).
//
// A published *Config is immutable. The data plane reads it through
// Store.Current without taking a lock, so every mutation builds a copy, writes
// it to disk first and only then swaps the pointer (design §1.7).
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
)

// Sentinel errors. The control plane maps them to status codes (design §6.4):
// ErrInvalidRetry and ErrInvalidConfig to 400, ErrRetryNotSet to 404,
// ErrRestartRequired to 409, ErrDirSync to 200 with a warning (the change is in
// effect), and any other error out of a persist step to 500.
var (
	ErrInvalidConfig   = errors.New("config: invalid configuration")
	ErrInvalidRetry    = errors.New("config: invalid retry max")
	ErrRetryNotSet     = errors.New("config: model has no per-model retry max")
	ErrRestartRequired = errors.New("config: change takes effect only after a restart")
	ErrDirSync         = errors.New("config: renamed into place but directory fsync failed")
)

// maxStreakCap bounds gate.streak_cap so that base << (streak_cap-1) cannot
// overflow int64 nanoseconds: base is at most Caddy's response_header_timeout
// (30m = 1.8e12ns) and 1.8e12 << 22 ≈ 7.5e18 < 2^63 (design §6.4.1).
const maxStreakCap = 23

// syncDirFn is the directory fsync step of writeFileAtomic. Tests replace it to
// exercise the ErrDirSync path, which a real filesystem rarely produces.
var syncDirFn = syncDir

// Config mirrors /etc/nahook/config.json (design §5).
type Config struct {
	Listen        string   `json:"listen"`
	ControlSocket string   `json:"control_socket"`
	NewAPI        string   `json:"newapi"`
	BodyLimit     int64    `json:"body_limit"`
	Match         Match    `json:"match"`
	Retry         Retry    `json:"retry"`
	Gate          GateConf `json:"gate"`
}

// Match selects the requests that may be intercepted (design §1.2). An empty
// Models list means every streaming request (design §5).
type Match struct {
	Paths  []string `json:"paths"`
	Models []string `json:"models"`
}

// Retry is the retry ceiling: Max applies to every model, PerModel overrides it
// for the models it names. A model missing from PerModel uses Max (§1.7).
type Retry struct {
	Max      int            `json:"max"`
	PerModel map[string]int `json:"per_model"`
}

// GateConf parameterises the cooldown gate (design §1.5). StreakCap is int32
// because (*gate.Gate).OnRateLimit takes an int32 ceiling.
type GateConf struct {
	BaseFloorMS int64 `json:"base_floor_ms"`
	StreakCap   int32 `json:"streak_cap"`
}

// Validate reports whether c can be served, following the rule table in design
// §6.4.1: an empty listen address or control socket, a newapi base that is not
// an absolute http(s) URL, a non-positive body limit, a negative gate floor, a
// streak cap outside 1..maxStreakCap, a negative retry ceiling, and a per-model
// entry whose key is empty or "*" (the global scope) are all rejected.
//
// Load does not call Validate: the documented order is Load → Validate →
// NewStore (interface §1), so a bad file never becomes a snapshot.
func (c *Config) Validate() error {
	switch {
	case c.Listen == "":
		return fmt.Errorf("%w: listen is empty", ErrInvalidConfig)
	case c.ControlSocket == "":
		return fmt.Errorf("%w: control_socket is empty", ErrInvalidConfig)
	}
	u, err := url.Parse(c.NewAPI)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%w: newapi %q is not an absolute http(s) URL", ErrInvalidConfig, c.NewAPI)
	}
	if c.BodyLimit <= 0 {
		return fmt.Errorf("%w: body_limit %d must be positive", ErrInvalidConfig, c.BodyLimit)
	}
	if c.Gate.BaseFloorMS < 0 {
		return fmt.Errorf("%w: gate.base_floor_ms %d must not be negative", ErrInvalidConfig, c.Gate.BaseFloorMS)
	}
	if c.Gate.StreakCap < 1 || c.Gate.StreakCap > maxStreakCap {
		return fmt.Errorf("%w: gate.streak_cap %d must be within 1..%d", ErrInvalidConfig, c.Gate.StreakCap, maxStreakCap)
	}
	if c.Retry.Max < 0 {
		return fmt.Errorf("%w: retry.max %d must not be negative", ErrInvalidRetry, c.Retry.Max)
	}
	for model, n := range c.Retry.PerModel {
		if model == "" || model == "*" {
			return fmt.Errorf("%w: retry.per_model key %q is reserved", ErrInvalidRetry, model)
		}
		if n < 0 {
			return fmt.Errorf("%w: retry.per_model[%q]=%d must not be negative", ErrInvalidRetry, model, n)
		}
	}
	return nil
}

// Load reads and parses one configuration file. It does not validate the
// result; the caller runs (*Config).Validate. Unknown keys and anything after
// the top-level object are rejected, so a misspelt key cannot silently fall
// back to a zero value (design §6.4.1).
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	c := new(Config)
	if err := dec.Decode(c); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("config: parse %s: unexpected data after the configuration object", path)
	}
	return c, nil
}

// Persist writes c to path atomically: a temporary file in the same directory,
// fsync of the file, rename, then fsync of the directory (design §8). The
// configuration holds the control socket path and the upstream address, so the
// file is created 0600 (design §5). Empty collections are written as {} and []
// to match the design §5 sample. A directory fsync failure is reported as
// ErrDirSync: the rename has already happened and the new content is in place.
func Persist(path string, c *Config) error {
	out := *c
	if out.Match.Paths == nil {
		out.Match.Paths = []string{}
	}
	if out.Match.Models == nil {
		out.Match.Models = []string{}
	}
	if out.Retry.PerModel == nil {
		out.Retry.PerModel = map[string]int{}
	}
	buf, err := json.MarshalIndent(&out, "", "  ")
	if err != nil {
		return fmt.Errorf("config: marshal: %w", err)
	}
	buf = append(buf, '\n')
	return writeFileAtomic(path, buf)
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("config: create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	discard := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	if _, err := tmp.Write(data); err != nil {
		discard()
		return fmt.Errorf("config: write %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		discard()
		return fmt.Errorf("config: chmod %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		discard()
		return fmt.Errorf("config: sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("config: close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("config: rename %s to %s: %w", tmpName, path, err)
	}
	if err := syncDirFn(dir); err != nil {
		return fmt.Errorf("%w: %v", ErrDirSync, err)
	}
	return nil
}

// syncDir makes the rename durable. Windows cannot flush a directory handle, so
// there the step is a no-op; the build target is linux/amd64 (design §7).
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("config: open dir %s: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		if runtime.GOOS == "windows" {
			return nil
		}
		return fmt.Errorf("config: sync dir %s: %w", dir, err)
	}
	return nil
}
