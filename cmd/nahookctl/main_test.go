package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ARYYDS/NaHook/internal/config"
	"github.com/ARYYDS/NaHook/internal/control"
	"github.com/ARYYDS/NaHook/internal/gate"
	"github.com/ARYYDS/NaHook/internal/obs"
)

func testConfig() *config.Config {
	return &config.Config{
		Listen:        "127.0.0.1:19080",
		ControlSocket: "/run/nahook/ctl.sock",
		NewAPI:        "http://127.0.0.1:3000",
		BodyLimit:     64 << 20,
		Match: config.Match{
			Paths:  []string{"/v1/responses", "/v1/chat/completions"},
			Models: []string{"gpt-6-astra"},
		},
		Retry: config.Retry{Max: 3, PerModel: map[string]int{}},
		Gate:  config.GateConf{BaseFloorMS: 50, StreakCap: 3},
	}
}

// startControl runs a real control server on a short unix socket path (sun_path
// is 108 bytes, so t.TempDir() is not always short enough). It also returns the
// configuration file, so a test can edit it the way an operator would.
func startControl(t *testing.T) (socket, configPath string) {
	t.Helper()

	dir, err := os.MkdirTemp("", "nctl")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket = filepath.Join(dir, "s")

	configPath = filepath.Join(t.TempDir(), "config.json")
	cfg := testConfig()
	if err := config.Persist(configPath, cfg); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	store := config.NewStore(configPath, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		errc <- control.Serve(ctx, socket, store, gate.NewRegistry(), obs.NewStats(), obs.NewLogger(io.Discard))
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errc:
			if err != nil {
				t.Errorf("control.Serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("control.Serve did not stop")
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.Dial("unix", socket)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("control socket never came up: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return socket, configPath
}

func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestRetryRoundTrip(t *testing.T) {
	socket, _ := startControl(t)

	code, stdout, stderr := runCLI(t, "retry", "get", "--socket", socket)
	if code != 0 {
		t.Fatalf("get: code = %d, stderr = %s", code, stderr)
	}
	if want := `{"scope":"*","max":3,"source":"global"}` + "\n"; stdout != want {
		t.Fatalf("get stdout = %q, want %q", stdout, want)
	}

	code, stdout, stderr = runCLI(t, "retry", "set", "5", "--model", "gpt-6-astra", "--socket", socket)
	if code != 0 {
		t.Fatalf("set: code = %d, stderr = %s", code, stderr)
	}
	if want := `{"scope":"gpt-6-astra","old":3,"new":5}` + "\n"; stdout != want {
		t.Fatalf("set stdout = %q, want %q", stdout, want)
	}

	code, stdout, _ = runCLI(t, "retry", "get", "--model", "gpt-6-astra", "--socket", socket)
	if code != 0 {
		t.Fatalf("get model: code = %d", code)
	}
	if want := `{"scope":"gpt-6-astra","max":5,"source":"per_model"}` + "\n"; stdout != want {
		t.Fatalf("get model stdout = %q, want %q", stdout, want)
	}

	code, stdout, stderr = runCLI(t, "retry", "unset", "--model", "gpt-6-astra", "--socket", socket)
	if code != 0 {
		t.Fatalf("unset: code = %d, stderr = %s", code, stderr)
	}
	if want := `{"scope":"gpt-6-astra","removed":true}` + "\n"; stdout != want {
		t.Fatalf("unset stdout = %q, want %q", stdout, want)
	}

	code, stdout, _ = runCLI(t, "retry", "get", "--model", "gpt-6-astra", "--socket", socket)
	if code != 0 {
		t.Fatalf("get after unset: code = %d", code)
	}
	if want := `{"scope":"gpt-6-astra","max":3,"source":"global"}` + "\n"; stdout != want {
		t.Fatalf("get after unset stdout = %q, want %q", stdout, want)
	}
}

func TestGlobalSetWithoutModel(t *testing.T) {
	socket, _ := startControl(t)

	code, stdout, stderr := runCLI(t, "retry", "set", "0", "--socket", socket)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr)
	}
	if want := `{"scope":"*","old":3,"new":0}` + "\n"; stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

func TestStatsAndReload(t *testing.T) {
	socket, _ := startControl(t)

	code, stdout, stderr := runCLI(t, "stats", "--socket", socket)
	if code != 0 {
		t.Fatalf("stats: code = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, `"counters"`) || !strings.Contains(stdout, `"gates"`) {
		t.Fatalf("stats stdout = %q", stdout)
	}
	if !strings.Contains(stdout, `"intercepted":0`) {
		t.Fatalf("stats stdout misses a zero counter: %q", stdout)
	}

	code, stdout, stderr = runCLI(t, "reload", "--socket", socket)
	if code != 0 {
		t.Fatalf("reload: code = %d, stderr = %s", code, stderr)
	}
	if want := `{"ok":true}` + "\n"; stdout != want {
		t.Fatalf("reload stdout = %q, want %q", stdout, want)
	}
}

func TestSocketFlagForms(t *testing.T) {
	socket, _ := startControl(t)

	if code, _, stderr := runCLI(t, "--socket="+socket, "stats"); code != 0 {
		t.Fatalf("--socket=path before the command: code = %d, stderr = %s", code, stderr)
	}
	if code, _, stderr := runCLI(t, "stats", "--socket="+socket); code != 0 {
		t.Fatalf("--socket=path after the command: code = %d, stderr = %s", code, stderr)
	}
}

func TestUsageErrors(t *testing.T) {
	tests := [][]string{
		{},
		{"bogus"},
		{"retry"},
		{"retry", "bogus"},
		{"retry", "get", "extra"},
		{"retry", "set"},
		{"retry", "set", "abc"},
		{"retry", "set", "1", "extra"},
		{"retry", "unset"},
		{"retry", "unset", "--model="},
		{"stats", "extra"},
		{"reload", "extra"},
		{"config"},
		{"config", "bogus"},
		{"config", "show", "extra"},
		{"--socket"},
		{"--bogus", "stats"},
	}

	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, stdout, stderr := runCLI(t, args...)
			if code != 2 {
				t.Fatalf("code = %d, want 2 (stdout %q, stderr %q)", code, stdout, stderr)
			}
			if stderr == "" {
				t.Fatal("no message on stderr")
			}
			if stdout != "" {
				t.Fatalf("stdout = %q, want empty", stdout)
			}
		})
	}
}

func TestHelp(t *testing.T) {
	for _, arg := range []string{"--help", "-h", "help"} {
		code, stdout, _ := runCLI(t, arg)
		if code != 0 {
			t.Fatalf("%s: code = %d, want 0", arg, code)
		}
		if !strings.Contains(stdout, "usage: nahookctl") {
			t.Fatalf("%s: stdout = %q", arg, stdout)
		}
	}
}

func TestControlErrors(t *testing.T) {
	t.Run("rejected value", func(t *testing.T) {
		socket, _ := startControl(t)
		code, stdout, stderr := runCLI(t, "retry", "set", "-1", "--socket", socket)
		if code != 1 {
			t.Fatalf("code = %d, want 1 (stdout %q)", code, stdout)
		}
		if !strings.Contains(stderr, "control returned 400") {
			t.Fatalf("stderr = %q", stderr)
		}
	})

	t.Run("nothing to unset", func(t *testing.T) {
		socket, _ := startControl(t)
		code, _, stderr := runCLI(t, "retry", "unset", "--model", "gpt-6-lunna", "--socket", socket)
		if code != 1 {
			t.Fatalf("code = %d, want 1", code)
		}
		if !strings.Contains(stderr, "control returned 404") {
			t.Fatalf("stderr = %q", stderr)
		}
	})

	t.Run("no control socket", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "absent")
		code, _, stderr := runCLI(t, "stats", "--socket", missing)
		if code != 1 {
			t.Fatalf("code = %d, want 1", code)
		}
		if !strings.Contains(stderr, "control socket") {
			t.Fatalf("stderr = %q", stderr)
		}
	})
}

// TestConfigShow prints the control plane's answer verbatim: the effective
// configuration plus whether the file still matches it (design §5, §6.5).
func TestConfigShow(t *testing.T) {
	socket, _ := startControl(t)

	code, stdout, stderr := runCLI(t, "config", "show", "--socket", socket)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr)
	}
	if !strings.HasSuffix(stdout, "\n") {
		t.Fatalf("stdout = %q, want the JSON line as the server sent it", stdout)
	}

	var answer struct {
		Config json.RawMessage `json:"config"`
		File   string          `json:"file"`
	}
	if err := json.Unmarshal([]byte(stdout), &answer); err != nil {
		t.Fatalf("stdout is not JSON: %v (%q)", err, stdout)
	}
	if answer.File != "in_sync" {
		t.Fatalf("file = %q, want in_sync", answer.File)
	}
	if got := string(answer.Config); !strings.Contains(got, `"max":3`) || !strings.Contains(got, `"listen"`) {
		t.Fatalf("config = %s, want the effective configuration", got)
	}
}

// TestConfigConflict is the operator workflow of T-F18: a hand edit that was not
// reloaded makes retry set exit 1 with 409, config show reports "changed", and
// after a reload the same write succeeds without losing the edit.
func TestConfigConflict(t *testing.T) {
	socket, path := startControl(t)

	edited, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	edited.Match.Models = append(edited.Match.Models, "gpt-6-lunna")
	if err := config.Persist(path, edited); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	code, _, stderr := runCLI(t, "retry", "set", "3", "--socket", socket)
	if code != 1 {
		t.Fatalf("retry set with a changed file: code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "409") || !strings.Contains(stderr, "reload") {
		t.Fatalf("stderr = %q, want the 409 and the reload hint", stderr)
	}

	code, stdout, stderr := runCLI(t, "config", "show", "--socket", socket)
	if code != 0 {
		t.Fatalf("config show: code = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, `"file":"changed"`) {
		t.Fatalf("stdout = %q, want file changed", stdout)
	}

	if code, _, stderr := runCLI(t, "reload", "--socket", socket); code != 0 {
		t.Fatalf("reload: code = %d, stderr = %s", code, stderr)
	}
	if code, _, stderr := runCLI(t, "retry", "set", "3", "--socket", socket); code != 0 {
		t.Fatalf("retry set after reload: code = %d, stderr = %s", code, stderr)
	}

	after, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(after.Match.Models) != 2 || after.Match.Models[1] != "gpt-6-lunna" {
		t.Fatalf("models = %v, want the hand-added model to survive", after.Match.Models)
	}
}
