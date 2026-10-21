package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ARYYDS/NaHook/internal/config"
	"github.com/ARYYDS/NaHook/internal/gate"
	"github.com/ARYYDS/NaHook/internal/obs"
)

func validConfig() *config.Config {
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

// newStore persists a valid config into a temp directory and returns the store
// plus the file path, so a test can rewrite the file behind the store's back.
func newStore(t *testing.T) (*config.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := validConfig()
	if err := config.Persist(path, cfg); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	return config.NewStore(path, cfg), path
}

// shortSocket returns a socket path short enough for sun_path (108 bytes);
// t.TempDir() under a long GOTMPDIR would not fit on some systems.
func shortSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "nctl")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

// startControl runs Serve in the background and returns the socket path.
func startControl(t *testing.T, store *config.Store) (string, *gate.Registry, *obs.Stats) {
	t.Helper()
	socket := shortSocket(t)
	gates := gate.NewRegistry()
	stats := obs.NewStats()

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- Serve(ctx, socket, store, gates, stats) }()

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errc:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Serve did not stop after its context was cancelled")
		}
	})

	waitForSocket(t, socket)
	return socket, gates, stats
}

func waitForSocket(t *testing.T, socket string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.Dial("unix", socket)
		if err == nil {
			conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("control socket %s never accepted a connection: %v", socket, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func newClient(socket string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
		DisableKeepAlives: true,
	}}
}

func request(t *testing.T, client *http.Client, method, path, body string) (int, string) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, "http://nahook"+path, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(b)
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := new(bytes.Buffer)
	previous := slog.Default()
	slog.SetDefault(obs.NewLogger(buf))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return buf
}

func newRecorder() *httptest.ResponseRecorder {
	return httptest.NewRecorder()
}

// findLogs returns every line with this message, in order.
func findLogs(t *testing.T, buf *bytes.Buffer, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("log line is not JSON: %v (%q)", err, line)
		}
		if event["msg"] == msg {
			out = append(out, event)
		}
	}
	return out
}

func findLog(t *testing.T, buf *bytes.Buffer, msg string) map[string]any {
	t.Helper()
	events := findLogs(t, buf, msg)
	if len(events) == 0 {
		t.Fatalf("no %q line in logs:\n%s", msg, buf.String())
	}
	return events[0]
}

func TestGetRetry(t *testing.T) {
	store, _ := newStore(t)
	socket, _, _ := startControl(t, store)
	client := newClient(socket)

	status, body := request(t, client, http.MethodGet, "/v1/retry", "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if want := `{"scope":"*","max":3,"source":"global"}`; body != want {
		t.Fatalf("body = %s, want %s", body, want)
	}

	if status, body := request(t, client, http.MethodPut, "/v1/retry", `{"scope":"gpt-6-astra","n":1}`); status != http.StatusOK {
		t.Fatalf("PUT status = %d, body = %s", status, body)
	}

	status, body = request(t, client, http.MethodGet, "/v1/retry?model=gpt-6-astra", "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if want := `{"scope":"gpt-6-astra","max":1,"source":"per_model"}`; body != want {
		t.Fatalf("body = %s, want %s", body, want)
	}

	// A model without an override reports the global value and its source.
	status, body = request(t, client, http.MethodGet, "/v1/retry?model=gpt-6-lunna", "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if want := `{"scope":"gpt-6-lunna","max":3,"source":"global"}`; body != want {
		t.Fatalf("body = %s, want %s", body, want)
	}
}

func TestPutRetry(t *testing.T) {
	store, path := newStore(t)
	socket, _, _ := startControl(t, store)
	client := newClient(socket)

	status, body := request(t, client, http.MethodPut, "/v1/retry", `{"scope":"*","n":5}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if want := `{"scope":"*","old":3,"new":5}`; body != want {
		t.Fatalf("body = %s, want %s", body, want)
	}
	if got := store.RetryMax("anything"); got != 5 {
		t.Fatalf("RetryMax = %d, want 5", got)
	}
	onDisk, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if onDisk.Retry.Max != 5 {
		t.Fatalf("file retry.max = %d, want 5", onDisk.Retry.Max)
	}

	status, body = request(t, client, http.MethodPut, "/v1/retry", `{"scope":"gpt-6-astra","n":2}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if want := `{"scope":"gpt-6-astra","old":5,"new":2}`; body != want {
		t.Fatalf("body = %s, want %s", body, want)
	}
}

func TestPutRetryBadRequests(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "negative n", body: `{"scope":"*","n":-1}`},
		{name: "empty scope", body: `{"scope":"","n":3}`},
		{name: "broken JSON", body: `{"scope":`},
		{name: "not an object", body: `[1,2,3]`},
		{name: "missing n", body: `{"scope":"*"}`},
	}

	store, path := newStore(t)
	socket, _, _ := startControl(t, store)
	client := newClient(socket)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, body := request(t, client, http.MethodPut, "/v1/retry", tt.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", status, body)
			}
			var e errorBody
			if err := json.Unmarshal([]byte(body), &e); err != nil || e.Error == "" {
				t.Fatalf("body = %s", body)
			}
		})
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("a rejected request changed the configuration file")
	}
}

func TestDeleteRetry(t *testing.T) {
	store, _ := newStore(t)
	socket, _, _ := startControl(t, store)
	client := newClient(socket)

	if status, body := request(t, client, http.MethodPut, "/v1/retry", `{"scope":"gpt-6-astra","n":1}`); status != http.StatusOK {
		t.Fatalf("PUT status = %d, body = %s", status, body)
	}

	status, body := request(t, client, http.MethodDelete, "/v1/retry?model=gpt-6-astra", "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if want := `{"scope":"gpt-6-astra","removed":true}`; body != want {
		t.Fatalf("body = %s, want %s", body, want)
	}
	if got := store.RetryMax("gpt-6-astra"); got != 3 {
		t.Fatalf("RetryMax = %d, want the global 3", got)
	}

	status, body = request(t, client, http.MethodDelete, "/v1/retry?model=gpt-6-astra", "")
	if status != http.StatusNotFound {
		t.Fatalf("second DELETE status = %d, want 404 (body %s)", status, body)
	}

	status, body = request(t, client, http.MethodDelete, "/v1/retry", "")
	if status != http.StatusBadRequest {
		t.Fatalf("DELETE without a model: status = %d, want 400 (body %s)", status, body)
	}
}

func TestPutRetryWriteError(t *testing.T) {
	// The store points into a directory that does not exist, so Persist fails
	// before the rename: the answer is 500 and the snapshot stays untouched.
	path := filepath.Join(t.TempDir(), "missing", "config.json")
	store := config.NewStore(path, validConfig())
	socket, _, _ := startControl(t, store)
	client := newClient(socket)

	status, body := request(t, client, http.MethodPut, "/v1/retry", `{"scope":"*","n":5}`)
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %s)", status, body)
	}
	if got := store.RetryMax("gpt-6-astra"); got != 3 {
		t.Fatalf("RetryMax after a failed write = %d, want 3", got)
	}
}

func TestReload(t *testing.T) {
	store, path := newStore(t)
	socket, _, _ := startControl(t, store)
	client := newClient(socket)

	status, body := request(t, client, http.MethodPost, "/v1/reload", "")
	if status != http.StatusOK || body != `{"ok":true}` {
		t.Fatalf("status = %d, body = %s", status, body)
	}

	// A changed listen address cannot be applied without a restart: 409 and the
	// snapshot keeps the old value.
	changed := validConfig()
	changed.Listen = "127.0.0.1:19181"
	if err := config.Persist(path, changed); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	status, body = request(t, client, http.MethodPost, "/v1/reload", "")
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", status, body)
	}
	if got := store.Current().Listen; got != "127.0.0.1:19080" {
		t.Fatalf("snapshot listen = %q, want the old one", got)
	}

	// An invalid file is a bad request.
	if err := os.WriteFile(path, []byte(`{"listen":"127.0.0.1:19080","control_socket":"/run/x","newapi":"http://127.0.0.1:3000","body_limit":1,"retry":{"max":-1},"gate":{"streak_cap":3}}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	status, body = request(t, client, http.MethodPost, "/v1/reload", "")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", status, body)
	}

	// A missing file is a server-side failure.
	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	status, body = request(t, client, http.MethodPost, "/v1/reload", "")
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %s)", status, body)
	}
}

func TestStats(t *testing.T) {
	store, _ := newStore(t)
	socket, gates, stats := startControl(t, store)
	client := newClient(socket)

	stats.Inc(obs.Retries)
	stats.Inc(obs.Retries)
	gates.Get("gpt-6-astra|org-mock").OnRateLimit(30*time.Millisecond, 20*time.Millisecond, 3)

	status, body := request(t, client, http.MethodGet, "/v1/stats", "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}

	var view struct {
		Counters map[string]int64 `json:"counters"`
		Gates    []gate.GateStat  `json:"gates"`
	}
	if err := json.Unmarshal([]byte(body), &view); err != nil {
		t.Fatalf("decode: %v (body %s)", err, body)
	}
	if len(view.Counters) != 8 {
		t.Fatalf("counters = %v, want 8 keys", view.Counters)
	}
	if view.Counters["retries"] != 2 || view.Counters["intercepted"] != 0 {
		t.Fatalf("counters = %v", view.Counters)
	}
	if len(view.Gates) != 1 || view.Gates[0].Key != "gpt-6-astra|org-mock" || view.Gates[0].Streak != 1 {
		t.Fatalf("gates = %+v", view.Gates)
	}
}

// TestDirSyncWarning covers the ErrDirSync branch: the change is already in the
// file, so the answer is 200 with a warning. A real ErrDirSync can only come
// from a Linux directory fsync failure, so the response writer is tested here
// directly instead of through a socket.
func TestDirSyncWarning(t *testing.T) {
	dirErr := fmt.Errorf("config: sync dir: %w", config.ErrDirSync)

	rec := newRecorder()
	writeRetryChange(rec, "gpt-6-astra", 3, 1, dirErr)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != `{"scope":"gpt-6-astra","old":3,"new":1,"warning":"`+dirSyncWarning+`"}` {
		t.Fatalf("PUT body = %s", got)
	}

	rec = newRecorder()
	writeRemoved(rec, "gpt-6-astra", dirErr)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != `{"scope":"gpt-6-astra","removed":true,"warning":"`+dirSyncWarning+`"}` {
		t.Fatalf("DELETE body = %s", got)
	}
}

func TestErrorMapping(t *testing.T) {
	tests := []struct {
		err  error
		want int
	}{
		{err: config.ErrInvalidRetry, want: http.StatusBadRequest},
		{err: config.ErrInvalidConfig, want: http.StatusBadRequest},
		{err: config.ErrRetryNotSet, want: http.StatusNotFound},
		{err: config.ErrRestartRequired, want: http.StatusConflict},
		{err: errors.New("disk on fire"), want: http.StatusInternalServerError},
	}
	for _, tt := range tests {
		rec := newRecorder()
		writeStoreError(rec, tt.err)
		if rec.Code != tt.want {
			t.Fatalf("%v: status = %d, want %d", tt.err, rec.Code, tt.want)
		}
		var e errorBody
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Error != tt.err.Error() {
			t.Fatalf("%v: body = %s", tt.err, rec.Body.String())
		}
	}
}

func TestLogLines(t *testing.T) {
	logs := captureLogs(t)
	store, _ := newStore(t)
	socket, _, _ := startControl(t, store)
	client := newClient(socket)

	wantUID := "-"
	if runtime.GOOS == "linux" {
		wantUID = fmt.Sprint(os.Getuid())
	}

	if status, body := request(t, client, http.MethodPut, "/v1/retry", `{"scope":"*","n":4}`); status != http.StatusOK {
		t.Fatalf("PUT status = %d, body = %s", status, body)
	}
	if status, body := request(t, client, http.MethodPut, "/v1/retry", `{"scope":"gpt-6-astra","n":1}`); status != http.StatusOK {
		t.Fatalf("PUT status = %d, body = %s", status, body)
	}
	if status, body := request(t, client, http.MethodDelete, "/v1/retry?model=gpt-6-astra", ""); status != http.StatusOK {
		t.Fatalf("DELETE status = %d, body = %s", status, body)
	}
	if status, body := request(t, client, http.MethodPost, "/v1/reload", ""); status != http.StatusOK {
		t.Fatalf("reload status = %d, body = %s", status, body)
	}

	changes := findLogs(t, logs, "config.retry_max_changed")
	want := []struct {
		scope    string
		old, new float64
	}{
		{scope: "*", old: 3, new: 4},
		{scope: "gpt-6-astra", old: 4, new: 1},
		{scope: "gpt-6-astra", old: 1, new: 4}, // unset falls back to the global 4
	}
	if len(changes) != len(want) {
		t.Fatalf("retry_max_changed lines = %d, want %d:\n%s", len(changes), len(want), logs.String())
	}
	for i, w := range want {
		if changes[i]["scope"] != w.scope || changes[i]["old"] != w.old || changes[i]["new"] != w.new {
			t.Fatalf("line %d = %v, want scope=%s old=%v new=%v", i, changes[i], w.scope, w.old, w.new)
		}
		if changes[i]["uid"] != wantUID {
			t.Fatalf("line %d uid = %v, want %v", i, changes[i]["uid"], wantUID)
		}
	}

	if event := findLog(t, logs, "config.reloaded"); event["uid"] != wantUID {
		t.Fatalf("reloaded uid = %v, want %v", event["uid"], wantUID)
	}
}

func TestReloadFailureIsLogged(t *testing.T) {
	logs := captureLogs(t)
	store, path := newStore(t)
	socket, _, _ := startControl(t, store)
	client := newClient(socket)

	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if status, body := request(t, client, http.MethodPost, "/v1/reload", ""); status != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	event := findLog(t, logs, "config.reload_failed")
	if msg, _ := event["error"].(string); msg == "" {
		t.Fatalf("reload_failed has no error field: %v", event)
	}
}

func TestStaleSocketFileIsReplaced(t *testing.T) {
	store, _ := newStore(t)
	socket := shortSocket(t)
	if err := os.WriteFile(socket, []byte("stale"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- Serve(ctx, socket, store, gate.NewRegistry(), obs.NewStats()) }()
	t.Cleanup(func() {
		cancel()
		<-errc
	})

	waitForSocket(t, socket)
	if status, _ := request(t, newClient(socket), http.MethodGet, "/v1/retry", ""); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
}

func TestSocketMode(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("unix socket permissions only behave like this on Linux")
	}
	store, _ := newStore(t)
	socket, _, _ := startControl(t, store)

	info, err := os.Stat(socket)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("socket mode = %o, want 600", got)
	}
}
