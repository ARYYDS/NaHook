package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ARYYDS/NaHook/internal/config"
	"github.com/ARYYDS/NaHook/internal/gate"
	"github.com/ARYYDS/NaHook/internal/obs"
)

// rateLimitMessage is <RL> verbatim (渠道监控/429-vs-quota.md:12-14), the text
// the startup reader must recognise.
const rateLimitMessage = "Rate limit reached for gpt-6-astra in organization org-mock on tokens per min (TPM): " +
	"Limit 40000000, Used 40000000, Requested 19224. Please try again in 28ms."

const (
	createdFrame = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_mock\",\"status\":\"in_progress\"}}\n\n"
	deltaFrame   = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"Hello\"}\n\n"
	doneFrame    = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_mock\",\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":3,\"total_tokens\":13}}}\n\n"
)

// upstreamErrorFrame is the error payload of a rate-limit frame, shaped like
// <RLE> in design §6.6.
type upstreamErrorFrame struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func rateLimitFrames(message string) string {
	var frame upstreamErrorFrame
	frame.Type = "error"
	frame.Error.Type = "tokens"
	frame.Error.Code = "rate_limit_exceeded"
	frame.Error.Message = message
	body, _ := json.Marshal(frame)
	return createdFrame + "event: error\ndata: " + string(body) + "\n\n"
}

const http429Body = `{"error":{"message":"` + rateLimitMessage + `","type":"tokens","param":"","code":"rate_limit_exceeded"}}`

// upstreamRecord is one request the fake NewAPI received.
type upstreamRecord struct {
	host   string
	header http.Header
	body   []byte
}

// fakeUpstream is a scripted NewAPI: each request consumes the next script item
// and the last item repeats. It records what the proxy sent.
type fakeUpstream struct {
	server *httptest.Server

	mu      sync.Mutex
	script  []string
	records []upstreamRecord
	started chan struct{}
}

func newFakeUpstream(t *testing.T, script ...string) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{script: script, started: make(chan struct{}, 16)}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeUpstream) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)

	f.mu.Lock()
	seq := len(f.records) + 1
	item := ""
	if len(f.script) > 0 {
		item = f.script[0]
		if len(f.script) > 1 {
			f.script = f.script[1:]
		}
	} else {
		item = "ok"
	}
	rid := fmt.Sprintf("req-%d", seq)
	f.records = append(f.records, upstreamRecord{host: r.Host, header: r.Header.Clone(), body: body})
	f.mu.Unlock()
	select {
	case f.started <- struct{}{}:
	default:
	}

	if item == "hold" {
		<-r.Context().Done()
		return
	}

	h := w.Header()
	h.Set("X-Oneapi-Request-Id", rid)
	h.Set("Access-Control-Allow-Origin", "*")

	switch item {
	case "http429":
		h.Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, http429Body)
	case "ping_ok", "ping_final", "rate_limit", "final", "final_other", "ping_rate_limit":
		h.Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		if item == "ping_rate_limit" || item == "ping_ok" || item == "ping_final" {
			_, _ = io.WriteString(w, ": PING\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
		if item == "ping_ok" {
			_, _ = io.WriteString(w, createdFrame+deltaFrame+doneFrame)
			return
		}
		message := rateLimitMessage
		switch item {
		case "final", "ping_final":
			message += " (request id: " + rid + ")"
		case "final_other":
			message += " (request id: other-request)"
		}
		_, _ = io.WriteString(w, rateLimitFrames(message))
	default:
		h.Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, createdFrame+deltaFrame+doneFrame)
	}
}

func (f *fakeUpstream) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.records)
}

func (f *fakeUpstream) record(i int) upstreamRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.records[i]
}

// lockedBuffer collects the JSON log stream from concurrent handlers.
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

func testConfig(t *testing.T, newAPI string, retryMax int) *config.Config {
	t.Helper()
	c := &config.Config{
		Listen:        "127.0.0.1:0",
		ControlSocket: filepath.Join(t.TempDir(), "ctl.sock"),
		NewAPI:        newAPI,
		BodyLimit:     1 << 20,
		Match:         config.Match{Paths: []string{"/v1/responses"}},
		Retry:         config.Retry{Max: retryMax, PerModel: map[string]int{}},
		Gate:          config.GateConf{BaseFloorMS: 50, StreakCap: 3},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	return c
}

func newTestHandler(t *testing.T, c *config.Config) (*Handler, *obs.Stats, *gate.Registry, *lockedBuffer) {
	t.Helper()
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"), c)
	stats := obs.NewStats()
	gates := gate.NewRegistry()
	logs := &lockedBuffer{}
	h := NewHandler(store, gates, stats, obs.NewLogger(logs))
	return h, stats, gates, logs
}

// spyWriter records when the handler first touches the response, which must not
// happen before the decision is final (design §1.6).
type spyWriter struct {
	rec      *httptest.ResponseRecorder
	forwards func() int

	headerAt []int
	writes   int
	flushes  int
}

func (s *spyWriter) Header() http.Header { return s.rec.Header() }

func (s *spyWriter) WriteHeader(code int) {
	s.headerAt = append(s.headerAt, s.forwards())
	s.rec.WriteHeader(code)
}

func (s *spyWriter) Write(p []byte) (int, error) {
	s.writes++
	return s.rec.Write(p)
}

func (s *spyWriter) Flush() {
	s.flushes++
	s.rec.Flush()
}

func postRequest(ctx context.Context, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "http://client.example/v1/responses", strings.NewReader(body))
	r.Host = "client.example"
	return r.WithContext(ctx)
}

func streamBody(model string, stream bool) string {
	return fmt.Sprintf(`{"model":%q,"stream":%t,"input":"hi"}`, model, stream)
}

func assertCounters(t *testing.T, got map[string]int64, want map[string]int64) {
	t.Helper()
	for _, key := range []string{"intercepted", "rate_limited", "retries", "released", "gave_up", "passthrough", "bad_gateway", "client_gone"} {
		if got[key] != want[key] {
			t.Errorf("counter %s = %d, want %d", key, got[key], want[key])
		}
	}
	if got["rate_limited"] != got["retries"]+got["gave_up"] {
		t.Errorf("invariant broken: rate_limited=%d retries=%d gave_up=%d", got["rate_limited"], got["retries"], got["gave_up"])
	}
}

func TestInterceptGivesUpWithProxy429(t *testing.T) {
	up := newFakeUpstream(t, "rate_limit")
	c := testConfig(t, up.server.URL, 2)
	h, stats, _, _ := newTestHandler(t, c)

	spy := &spyWriter{rec: httptest.NewRecorder(), forwards: up.count}
	h.ServeHTTP(spy, postRequest(context.Background(), streamBody("gpt-6-astra", true)))

	if up.count() != 3 {
		t.Fatalf("upstream calls = %d, want 3", up.count())
	}
	if spy.rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", spy.rec.Code)
	}
	wantBody := `{"error":{"message":"` + rateLimitMessage + `","type":"tokens","param":"","code":"rate_limit_exceeded"}}`
	if spy.rec.Body.String() != wantBody {
		t.Errorf("body = %q, want %q", spy.rec.Body.String(), wantBody)
	}
	if got := spy.rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := spy.rec.Header().Get("X-Oneapi-Request-Id"); got != "req-3" {
		t.Errorf("X-Oneapi-Request-Id = %q, want req-3", got)
	}
	if got := spy.rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}
	if len(spy.headerAt) != 1 {
		t.Fatalf("WriteHeader calls = %d, want 1", len(spy.headerAt))
	}
	if spy.headerAt[0] != 3 {
		t.Errorf("WriteHeader after %d upstream calls, want 3 (nothing may be written before the decision)", spy.headerAt[0])
	}
	assertCounters(t, stats.Snapshot(), map[string]int64{
		"intercepted": 1, "rate_limited": 3, "retries": 2, "gave_up": 1,
	})
}

func TestInterceptRetriesThenReleases(t *testing.T) {
	up := newFakeUpstream(t, "rate_limit", "ok")
	c := testConfig(t, up.server.URL, 3)
	h, stats, gates, _ := newTestHandler(t, c)
	// Seed a streak so the release below has something to clear.
	gates.Get("gpt-6-astra|org-mock").OnRateLimit(time.Millisecond, time.Millisecond, 3)

	spy := &spyWriter{rec: httptest.NewRecorder(), forwards: up.count}
	h.ServeHTTP(spy, postRequest(context.Background(), streamBody("gpt-6-astra", true)))

	if up.count() != 2 {
		t.Fatalf("upstream calls = %d, want 2", up.count())
	}
	if spy.rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", spy.rec.Code)
	}
	want := createdFrame + deltaFrame + doneFrame
	if spy.rec.Body.String() != want {
		t.Errorf("body = %q, want %q", spy.rec.Body.String(), want)
	}
	if len(spy.headerAt) != 1 || spy.headerAt[0] != 2 {
		t.Errorf("WriteHeader at %v, want exactly one after 2 upstream calls", spy.headerAt)
	}
	assertCounters(t, stats.Snapshot(), map[string]int64{
		"intercepted": 1, "rate_limited": 1, "retries": 1, "released": 1,
	})
	statsAfter := gates.Snapshot()
	if len(statsAfter) != 1 || statsAfter[0].Streak != 0 {
		t.Errorf("gate after release = %+v, want one key with streak 0", statsAfter)
	}
}

func TestInterceptPassesThroughHTTP429(t *testing.T) {
	up := newFakeUpstream(t, "http429")
	c := testConfig(t, up.server.URL, 3)
	h, stats, _, _ := newTestHandler(t, c)

	spy := &spyWriter{rec: httptest.NewRecorder(), forwards: up.count}
	h.ServeHTTP(spy, postRequest(context.Background(), streamBody("gpt-6-astra", true)))

	if up.count() != 1 {
		t.Fatalf("upstream calls = %d, want 1", up.count())
	}
	if spy.rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the upstream 429", spy.rec.Code)
	}
	if spy.rec.Body.String() != http429Body {
		t.Errorf("body = %q, want the upstream body unchanged", spy.rec.Body.String())
	}
	assertCounters(t, stats.Snapshot(), map[string]int64{"intercepted": 1, "passthrough": 1})
}

func TestServeHTTPPassesThroughNonStreaming(t *testing.T) {
	up := newFakeUpstream(t, "ok")
	c := testConfig(t, up.server.URL, 3)
	h, stats, _, _ := newTestHandler(t, c)

	spy := &spyWriter{rec: httptest.NewRecorder(), forwards: up.count}
	h.ServeHTTP(spy, postRequest(context.Background(), streamBody("gpt-6-astra", false)))

	if up.count() != 1 {
		t.Fatalf("upstream calls = %d, want 1", up.count())
	}
	assertCounters(t, stats.Snapshot(), map[string]int64{"passthrough": 1})
}

func TestServeHTTPPassesThroughOversizedBody(t *testing.T) {
	up := newFakeUpstream(t, "ok")
	c := testConfig(t, up.server.URL, 3)
	c.BodyLimit = 64
	h, stats, _, _ := newTestHandler(t, c)

	body := streamBody("gpt-6-astra", true) + strings.Repeat("x", 128)
	spy := &spyWriter{rec: httptest.NewRecorder(), forwards: up.count}
	h.ServeHTTP(spy, postRequest(context.Background(), body))

	if up.count() != 1 {
		t.Fatalf("upstream calls = %d, want 1", up.count())
	}
	rec := up.record(0)
	if string(rec.body) != body {
		t.Errorf("forwarded body differs: got %d bytes, want %d", len(rec.body), len(body))
	}
	assertCounters(t, stats.Snapshot(), map[string]int64{"passthrough": 1})
}

func TestForwardPreservesHeadersHostAndBody(t *testing.T) {
	up := newFakeUpstream(t, "rate_limit", "ok")
	c := testConfig(t, up.server.URL, 1)
	h, _, _, _ := newTestHandler(t, c)

	req := postRequest(context.Background(), streamBody("gpt-6-astra", true))
	req.Header.Set("Connection", "X-Custom-Hop")
	req.Header.Set("X-Custom-Hop", "drop-me")
	req.Header.Set("Keep-Alive", "timeout=5")
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	req.Header.Set("X-Real-IP", "203.0.113.9")
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("X-Trace", "keep-me")

	spy := &spyWriter{rec: httptest.NewRecorder(), forwards: up.count}
	h.ServeHTTP(spy, req)

	if up.count() != 2 {
		t.Fatalf("upstream calls = %d, want 2", up.count())
	}
	first := up.record(0)
	if first.host != "client.example" {
		t.Errorf("Host = %q, want client.example", first.host)
	}
	for _, name := range []string{"Connection", "X-Custom-Hop", "Keep-Alive"} {
		if got := first.header.Get(name); got != "" {
			t.Errorf("hop-by-hop header %s leaked: %q", name, got)
		}
	}
	for name, want := range map[string]string{
		"X-Forwarded-For": "203.0.113.9",
		"X-Real-IP":       "203.0.113.9",
		"Authorization":   "Bearer sk-test",
		"X-Trace":         "keep-me",
		"Accept-Encoding": "identity",
	} {
		if got := first.header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if string(first.body) != streamBody("gpt-6-astra", true) {
		t.Errorf("first body = %q", first.body)
	}
	if string(up.record(1).body) != string(first.body) {
		t.Errorf("retry body differs from the first attempt")
	}
}

func TestNewAPIFinalErrorIsPassedThrough(t *testing.T) {
	up := newFakeUpstream(t, "final")
	c := testConfig(t, up.server.URL, 3)
	h, stats, _, _ := newTestHandler(t, c)

	spy := &spyWriter{rec: httptest.NewRecorder(), forwards: up.count}
	h.ServeHTTP(spy, postRequest(context.Background(), streamBody("gpt-6-astra", true)))

	if up.count() != 1 {
		t.Fatalf("upstream calls = %d, want 1 (NewAPI's own final error must not be retried)", up.count())
	}
	if spy.rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", spy.rec.Code)
	}
	if !strings.Contains(spy.rec.Body.String(), " (request id: req-1)") {
		t.Errorf("body does not carry the original final error: %q", spy.rec.Body.String())
	}
	assertCounters(t, stats.Snapshot(), map[string]int64{"intercepted": 1, "passthrough": 1})
}

func TestForeignRequestIDIsStillIntercepted(t *testing.T) {
	up := newFakeUpstream(t, "final_other")
	c := testConfig(t, up.server.URL, 0)
	h, stats, _, _ := newTestHandler(t, c)

	spy := &spyWriter{rec: httptest.NewRecorder(), forwards: up.count}
	h.ServeHTTP(spy, postRequest(context.Background(), streamBody("gpt-6-astra", true)))

	if up.count() != 1 {
		t.Fatalf("upstream calls = %d, want 1", up.count())
	}
	if spy.rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", spy.rec.Code)
	}
	assertCounters(t, stats.Snapshot(), map[string]int64{"intercepted": 1, "rate_limited": 1, "gave_up": 1})
}

func TestPingIsKeptInReleasedStream(t *testing.T) {
	up := newFakeUpstream(t, "rate_limit", "ping_ok")
	c := testConfig(t, up.server.URL, 3)
	h, _, _, _ := newTestHandler(t, c)

	spy := &spyWriter{rec: httptest.NewRecorder(), forwards: up.count}
	h.ServeHTTP(spy, postRequest(context.Background(), streamBody("gpt-6-astra", true)))

	if up.count() != 2 {
		t.Fatalf("upstream calls = %d, want 2", up.count())
	}
	body := spy.rec.Body.String()
	if !strings.HasPrefix(body, ": PING\n\n"+createdFrame+deltaFrame) {
		t.Errorf("released stream does not keep the PING and the successful attempt: %q", body)
	}
}

func TestPingWithFinalErrorIsPassedThrough(t *testing.T) {
	up := newFakeUpstream(t, "ping_final")
	c := testConfig(t, up.server.URL, 3)
	h, stats, _, _ := newTestHandler(t, c)

	spy := &spyWriter{rec: httptest.NewRecorder(), forwards: up.count}
	h.ServeHTTP(spy, postRequest(context.Background(), streamBody("gpt-6-astra", true)))

	if up.count() != 1 {
		t.Fatalf("upstream calls = %d, want 1", up.count())
	}
	body := spy.rec.Body.String()
	if !strings.HasPrefix(body, ": PING\n\n") || !strings.Contains(body, " (request id: req-1)") {
		t.Errorf("final error was not passed through unchanged: %q", body)
	}
	assertCounters(t, stats.Snapshot(), map[string]int64{"intercepted": 1, "passthrough": 1})
}

func TestForwardFailureWritesBadGateway(t *testing.T) {
	// Port 1 on loopback is closed, so the dial fails.
	c := testConfig(t, "http://127.0.0.1:1", 3)
	h, stats, _, _ := newTestHandler(t, c)

	spy := &spyWriter{rec: httptest.NewRecorder(), forwards: func() int { return 0 }}
	h.ServeHTTP(spy, postRequest(context.Background(), streamBody("gpt-6-astra", true)))

	if spy.rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", spy.rec.Code)
	}
	want := `{"error":{"message":"nahook: upstream unavailable","type":"nahook_error","param":"","code":"upstream_unavailable"}}`
	if spy.rec.Body.String() != want {
		t.Errorf("body = %q, want %q", spy.rec.Body.String(), want)
	}
	assertCounters(t, stats.Snapshot(), map[string]int64{"intercepted": 1, "bad_gateway": 1})
}

func TestClientGoneDuringForward(t *testing.T) {
	up := newFakeUpstream(t, "hold")
	c := testConfig(t, up.server.URL, 3)
	h, stats, _, _ := newTestHandler(t, c)

	ctx, cancel := context.WithCancel(context.Background())
	spy := &spyWriter{rec: httptest.NewRecorder(), forwards: up.count}
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(spy, postRequest(ctx, streamBody("gpt-6-astra", true)))
		close(done)
	}()

	select {
	case <-up.started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never received the request")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after the client left")
	}

	if spy.rec.Code != http.StatusOK || spy.rec.Body.Len() != 0 {
		t.Errorf("handler wrote a response after the client left: code=%d body=%q", spy.rec.Code, spy.rec.Body.String())
	}
	assertCounters(t, stats.Snapshot(), map[string]int64{"intercepted": 1, "client_gone": 1})
}

// roundTripFunc lets a test replace the transport, which is the only way to
// cancel the request context exactly between the startup read and the wait.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// cancelOnRead cancels the request as soon as the startup reader touches the
// body, so the gate wait already sees a canceled context.
type cancelOnRead struct {
	once   sync.Once
	cancel context.CancelFunc
	r      *strings.Reader
}

func (c *cancelOnRead) Read(p []byte) (int, error) {
	c.once.Do(c.cancel)
	return c.r.Read(p)
}

func TestClientGoneDuringWait(t *testing.T) {
	up := newFakeUpstream(t, "rate_limit")
	c := testConfig(t, up.server.URL, 3)
	h, stats, _, _ := newTestHandler(t, c)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	h.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "X-Oneapi-Request-Id": []string{"req-x"}},
			Body:       io.NopCloser(&cancelOnRead{cancel: cancel, r: strings.NewReader(rateLimitFrames(rateLimitMessage))}),
		}
		return resp, nil
	})}

	spy := &spyWriter{rec: httptest.NewRecorder(), forwards: func() int { return int(calls.Load()) }}
	h.ServeHTTP(spy, postRequest(ctx, streamBody("gpt-6-astra", true)))

	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1 (no retry after the client left)", calls.Load())
	}
	if spy.rec.Code != http.StatusOK || spy.rec.Body.Len() != 0 {
		t.Errorf("handler wrote a response after the client left: code=%d body=%q", spy.rec.Code, spy.rec.Body.String())
	}
	assertCounters(t, stats.Snapshot(), map[string]int64{"intercepted": 1, "rate_limited": 1, "retries": 1, "client_gone": 1})
}

func TestLogsFollowDesign(t *testing.T) {
	up := newFakeUpstream(t, "rate_limit", "ok")
	c := testConfig(t, up.server.URL, 3)
	h, _, _, logs := newTestHandler(t, c)

	spy := &spyWriter{rec: httptest.NewRecorder(), forwards: up.count}
	h.ServeHTTP(spy, postRequest(context.Background(), streamBody("gpt-6-astra", true)))

	events := map[string]map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", line, err)
		}
		msg, _ := entry["msg"].(string)
		events[msg] = entry
	}

	for _, name := range []string{"intercept", "wait", "release"} {
		if _, ok := events[name]; !ok {
			t.Fatalf("missing %q event in %s", name, logs.String())
		}
	}
	for _, field := range []string{"request_id", "key", "attempt", "elapsed_ms", "max"} {
		if _, ok := events["intercept"][field]; !ok {
			t.Errorf("intercept is missing %q: %v", field, events["intercept"])
		}
	}
	if events["intercept"]["key"] != "gpt-6-astra|org-mock" {
		t.Errorf("intercept key = %v", events["intercept"]["key"])
	}
	for _, field := range []string{"key", "wait_ms"} {
		if _, ok := events["wait"][field]; !ok {
			t.Errorf("wait is missing %q", field)
		}
	}
	for _, field := range []string{"request_id", "attempt"} {
		if _, ok := events["release"][field]; !ok {
			t.Errorf("release is missing %q", field)
		}
	}

	up2 := newFakeUpstream(t, "rate_limit")
	c2 := testConfig(t, up2.server.URL, 0)
	h2, _, _, logs2 := newTestHandler(t, c2)
	h2.ServeHTTP(&spyWriter{rec: httptest.NewRecorder(), forwards: up2.count}, postRequest(context.Background(), streamBody("gpt-6-astra", true)))

	var giveUp map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs2.String()), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err == nil && entry["msg"] == "give_up" {
			giveUp = entry
		}
	}
	if giveUp == nil {
		t.Fatalf("missing give_up event in %s", logs2.String())
	}
	for _, field := range []string{"request_id", "key", "attempts"} {
		if _, ok := giveUp[field]; !ok {
			t.Errorf("give_up is missing %q", field)
		}
	}
}
