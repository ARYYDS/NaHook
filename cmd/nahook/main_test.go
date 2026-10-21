package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ARYYDS/NaHook/internal/config"
)

const (
	rateLimitMessage = "Rate limit reached for gpt-6-astra in organization org-mock on tokens per min (TPM): " +
		"Limit 40000000, Used 40000000, Requested 19224. Please try again in 28ms."
	createdFrame = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_mock\",\"status\":\"in_progress\"}}\n\n"
	doneFrame    = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_mock\",\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":3,\"total_tokens\":13}}}\n\n"
)

func rateLimitFrames() string {
	return createdFrame + "event: error\ndata: " +
		`{"type":"error","error":{"type":"tokens","code":"rate_limit_exceeded","message":"` + rateLimitMessage + `"}}` + "\n\n"
}

// lockedBuffer collects the JSON log stream written by the proxy and the control
// plane at the same time.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// freeAddr reserves and releases a loopback port, so the process under test can
// bind it right after.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// shortSocketPath keeps the unix socket path short enough for every platform.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "nahook")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "ctl.sock")
}

func writeConfig(t *testing.T, c *config.Config) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.Persist(path, c); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	return path
}

func baseConfig(t *testing.T, newAPI, listen, socket string) *config.Config {
	t.Helper()
	return &config.Config{
		Listen:        listen,
		ControlSocket: socket,
		NewAPI:        newAPI,
		BodyLimit:     1 << 20,
		Match:         config.Match{Paths: []string{"/v1/responses"}},
		Retry:         config.Retry{Max: 1, PerModel: map[string]int{}},
		Gate:          config.GateConf{BaseFloorMS: 50, StreakCap: 3},
	}
}

// waitFor polls until check succeeds or the deadline passes.
func waitFor(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func controlClient(socket string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}
}

func TestRunRejectsBadConfig(t *testing.T) {
	sigs := make(chan os.Signal, 1)

	missing := filepath.Join(t.TempDir(), "absent.json")
	var stderr bytes.Buffer
	if code := runWithSignals([]string{"-config", missing}, io.Discard, &stderr, sigs); code != 1 {
		t.Fatalf("missing config exit = %d, want 1", code)
	}

	bad := baseConfig(t, "http://127.0.0.1:3000", "", shortSocketPath(t))
	path := writeConfig(t, bad)
	stderr.Reset()
	if code := runWithSignals([]string{"-config", path}, io.Discard, &stderr, sigs); code != 1 {
		t.Fatalf("invalid config exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "listen") {
		t.Errorf("stderr does not explain the failure: %q", stderr.String())
	}
}

func TestRunStartupFailureExitsOne(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	c := baseConfig(t, "http://127.0.0.1:3000", ln.Addr().String(), shortSocketPath(t))
	path := writeConfig(t, c)

	done := make(chan int, 1)
	go func() {
		done <- runWithSignals([]string{"-config", path}, io.Discard, io.Discard, make(chan os.Signal))
	}()

	select {
	case code := <-done:
		if code != 1 {
			t.Fatalf("exit = %d, want 1 when the data plane cannot listen", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("process did not exit after the listen failure")
	}
}

func TestRunControlFailureExitsOne(t *testing.T) {
	missingDir := filepath.Join(t.TempDir(), "no-such-dir")
	c := baseConfig(t, "http://127.0.0.1:3000", freeAddr(t), filepath.Join(missingDir, "ctl.sock"))
	path := writeConfig(t, c)

	done := make(chan int, 1)
	go func() {
		done <- runWithSignals([]string{"-config", path}, io.Discard, io.Discard, make(chan os.Signal))
	}()

	select {
	case code := <-done:
		if code != 1 {
			t.Fatalf("exit = %d, want 1 when the control socket cannot listen", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("process did not exit after the control failure")
	}
}

func TestRunSharesOneLoggerAndStopsOnSIGTERM(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Oneapi-Request-Id", fmt.Sprintf("req-%d", calls.Load()+1))
		w.WriteHeader(http.StatusOK)
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(w, rateLimitFrames())
			return
		}
		_, _ = io.WriteString(w, createdFrame+doneFrame)
	}))
	defer upstream.Close()

	addr := freeAddr(t)
	socket := shortSocketPath(t)
	c := baseConfig(t, upstream.URL, addr, socket)
	path := writeConfig(t, c)

	logs := &lockedBuffer{}
	sigs := make(chan os.Signal, 2)
	done := make(chan int, 1)
	go func() {
		done <- runWithSignals([]string{"-config", path}, logs, io.Discard, sigs)
	}()

	waitFor(t, "data plane", func() bool {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	})

	body := `{"model":"gpt-6-astra","stream":true}`
	resp, err := http.Post("http://"+addr+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if string(got) != createdFrame+doneFrame {
		t.Errorf("body = %q", got)
	}

	waitFor(t, "control socket", func() bool {
		_, err := os.Stat(socket)
		return err == nil
	})
	req, _ := http.NewRequest(http.MethodPut, "http://unix/v1/retry", strings.NewReader(`{"scope":"*","n":5}`))
	ctlResp, err := controlClient(socket).Do(req)
	if err != nil {
		t.Fatalf("control request: %v", err)
	}
	_ = ctlResp.Body.Close()
	if ctlResp.StatusCode != http.StatusOK {
		t.Fatalf("control status = %d, want 200", ctlResp.StatusCode)
	}

	waitFor(t, "both log streams", func() bool {
		out := logs.String()
		return strings.Contains(out, `"msg":"release"`) &&
			strings.Contains(out, `"msg":"config.retry_max_changed"`)
	})

	sigs <- syscall.SIGTERM
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit = %d, want 0 after SIGTERM", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("process did not stop after SIGTERM")
	}

	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
	}
}

func TestRunShutdownDrainsInflightRequest(t *testing.T) {
	started := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, createdFrame)
		if flusher != nil {
			flusher.Flush()
		}
		select {
		case started <- struct{}{}:
		default:
		}
		time.Sleep(200 * time.Millisecond)
		_, _ = io.WriteString(w, doneFrame)
	}))
	defer upstream.Close()

	addr := freeAddr(t)
	c := baseConfig(t, upstream.URL, addr, shortSocketPath(t))
	path := writeConfig(t, c)

	sigs := make(chan os.Signal, 2)
	done := make(chan int, 1)
	go func() {
		done <- runWithSignals([]string{"-config", path}, io.Discard, io.Discard, sigs)
	}()

	waitFor(t, "data plane", func() bool {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	})

	type result struct {
		body string
		err  error
	}
	results := make(chan result, 1)
	go func() {
		resp, err := http.Post("http://"+addr+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt-6-astra","stream":true}`))
		if err != nil {
			results <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, readErr := io.ReadAll(resp.Body)
		results <- result{body: string(b), err: readErr}
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never started streaming")
	}
	sigs <- syscall.SIGTERM

	select {
	case got := <-results:
		if got.err != nil {
			t.Fatalf("in-flight request failed during shutdown: %v", got.err)
		}
		if got.body != createdFrame+doneFrame {
			t.Errorf("in-flight response = %q, want the complete stream", got.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request did not finish")
	}

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("process did not stop")
	}

	if _, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		t.Error("data plane still accepts connections after shutdown")
	}
}

func TestRunRejectsUnknownFlag(t *testing.T) {
	if code := runWithSignals([]string{"-nope"}, io.Discard, io.Discard, make(chan os.Signal)); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
}
