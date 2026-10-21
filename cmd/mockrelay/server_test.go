package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ARYYDS/NaHook/internal/gate"
	"github.com/ARYYDS/NaHook/internal/startup"
)

// Golden bytes. They are written out here independently of frames.go on
// purpose: a change there must show up as a failing test, not as a moving
// reference (design §6.6: every frame is fixed).
const (
	wantRateLimitMessage = "Rate limit reached for gpt-6-astra in organization org-mock on tokens per min (TPM): " +
		"Limit 40000000, Used 40000000, Requested 19224. Please try again in 28ms."
	wantQuotaMessage = "You exceeded your current quota, please check your plan and billing details."

	wantRateLimitError = `{"type":"tokens","code":"rate_limit_exceeded","message":"` + wantRateLimitMessage + `"}`
	wantQuotaError     = `{"type":"insufficient_quota","code":"insufficient_quota","message":"` + wantQuotaMessage + `"}`

	createdJSON    = `{"type":"response.created","response":{"id":"resp_mock","object":"response","created_at":1760000000,"model":"gpt-6-astra","status":"in_progress"}}`
	deltaHelloJSON = `{"type":"response.output_text.delta","item_id":"msg_mock","output_index":0,"content_index":0,"delta":"Hello"}`
	deltaWorldJSON = `{"type":"response.output_text.delta","item_id":"msg_mock","output_index":0,"content_index":0,"delta":" world"}`
	deltaBangJSON  = `{"type":"response.output_text.delta","item_id":"msg_mock","output_index":0,"content_index":0,"delta":"!"}`
	completedJSON  = `{"type":"response.completed","response":{"id":"resp_mock","object":"response","created_at":1760000000,"model":"gpt-6-astra","status":"completed","usage":{"input_tokens":10,"output_tokens":3,"total_tokens":13}}}`

	chatHelloJSON = `{"id":"chatcmpl-mock","object":"chat.completion.chunk","created":1760000000,"model":"gpt-6-astra","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`
	chatWorldJSON = `{"id":"chatcmpl-mock","object":"chat.completion.chunk","created":1760000000,"model":"gpt-6-astra","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}`
	chatBangJSON  = `{"id":"chatcmpl-mock","object":"chat.completion.chunk","created":1760000000,"model":"gpt-6-astra","choices":[{"index":0,"delta":{"content":"!"},"finish_reason":null}]}`
	chatStopJSON  = `{"id":"chatcmpl-mock","object":"chat.completion.chunk","created":1760000000,"model":"gpt-6-astra","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13}}`

	wantResponsesOK = "event: response.created\ndata: " + createdJSON + "\n\n" +
		"event: response.output_text.delta\ndata: " + deltaHelloJSON + "\n\n" +
		"event: response.output_text.delta\ndata: " + deltaWorldJSON + "\n\n" +
		"event: response.output_text.delta\ndata: " + deltaBangJSON + "\n\n" +
		"event: response.completed\ndata: " + completedJSON + "\n\n"

	wantResponsesSlow1 = "event: response.created\ndata: " + createdJSON + "\n\n" +
		"event: response.output_text.delta\ndata: " + deltaHelloJSON + "\n\n" +
		"event: response.completed\ndata: " + completedJSON + "\n\n"

	wantChatOK = "data: " + chatHelloJSON + "\n\n" +
		"data: " + chatWorldJSON + "\n\n" +
		"data: " + chatBangJSON + "\n\n" +
		"data: " + chatStopJSON + "\n\n" +
		"data: [DONE]\n\n"

	wantRateLimit = "event: response.created\ndata: " + createdJSON + "\n\n" +
		"event: error\ndata: " + `{"type":"error","error":` + wantRateLimitError + `}` + "\n\n" +
		"event: response.failed\ndata: " +
		`{"type":"response.failed","response":{"id":"resp_mock","status":"failed","error":` + wantRateLimitError + `}}` + "\n\n"

	wantFailedOnly = "event: response.failed\ndata: " +
		`{"type":"response.failed","response":{"id":"resp_mock","status":"failed","error":` + wantRateLimitError + `}}` + "\n\n"

	wantQuota = "event: response.created\ndata: " + createdJSON + "\n\n" +
		"event: error\ndata: " + `{"type":"error","error":` + wantQuotaError + `}` + "\n\n" +
		"event: response.failed\ndata: " +
		`{"type":"response.failed","response":{"id":"resp_mock","status":"failed","error":` + wantQuotaError + `}}` + "\n\n"

	wantChatRateLimit = "data: " + `{"error":` + wantRateLimitError + `}` + "\n\n" + "data: [DONE]\n\n"

	wantHTTP429Body = `{"error":{"message":"` + wantRateLimitMessage + `","type":"tokens","param":"","code":"rate_limit_exceeded"}}`

	wantMismatchBody = `{"error":{"message":"mockrelay: item rate_limit does not apply to /v1/chat/completions"}}`
)

// --- helpers ---------------------------------------------------------------

func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	return newHandler()
}

func setScript(t *testing.T, h http.Handler, items ...string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string][]string{"script": items})
	if err != nil {
		t.Fatalf("marshal mode request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/__mode", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func mustSetScript(t *testing.T, h http.Handler, items ...string) {
	t.Helper()
	if rec := setScript(t, h, items...); rec.Code != http.StatusOK {
		t.Fatalf("/__mode %v: status = %d, body = %s", items, rec.Code, rec.Body.String())
	}
}

func business(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	return businessWithContext(t, h, path, context.Background())
}

func businessWithContext(t *testing.T, h http.Handler, path string, ctx context.Context) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"gpt-6-astra","stream":true}`)).
		WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func assertSSE(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	if got := rec.Body.String(); got != want {
		t.Fatalf("body mismatch:\n got %q\nwant %q", got, want)
	}
}

func readBody(t *testing.T, body []byte) startup.Result {
	t.Helper()
	res := startup.Read(bytes.NewReader(body))
	if res.Rest != nil {
		if _, err := io.Copy(io.Discard, res.Rest); err != nil {
			t.Fatalf("drain rest: %v", err)
		}
	}
	return res
}

func assertRateLimitVerdict(t *testing.T, body []byte) {
	t.Helper()
	res := readBody(t, body)
	if res.Verdict != startup.VerdictRateLimit {
		t.Fatalf("startup verdict = %v, want rate limit", res.Verdict)
	}
	if got := gate.Key("gpt-6-astra", res.Err.Message); got != "gpt-6-astra|org-mock" {
		t.Fatalf("gate.Key = %q, want gpt-6-astra|org-mock", got)
	}
}

func assertReleaseVerdict(t *testing.T, body []byte) {
	t.Helper()
	if res := readBody(t, body); res.Verdict != startup.VerdictRelease {
		t.Fatalf("startup verdict = %v, want release", res.Verdict)
	}
}

type logResponse struct {
	Count    int        `json:"count"`
	Requests []logEntry `json:"requests"`
}

func readLog(t *testing.T, h http.Handler) logResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/__log", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/__log: status = %d", rec.Code)
	}
	var out logResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("/__log decode: %v (body %s)", err, rec.Body.String())
	}
	if out.Count != len(out.Requests) {
		t.Fatalf("/__log count = %d, but %d requests", out.Count, len(out.Requests))
	}
	return out
}

// --- script items ----------------------------------------------------------

func TestOKResponses(t *testing.T) {
	h := newTestHandler(t)
	mustSetScript(t, h, "ok")

	rec := business(t, h, pathResponses)
	assertSSE(t, rec, wantResponsesOK)
	assertReleaseVerdict(t, rec.Body.Bytes())
}

func TestOKChat(t *testing.T) {
	h := newTestHandler(t)
	mustSetScript(t, h, "ok")

	rec := business(t, h, pathChat)
	assertSSE(t, rec, wantChatOK)
	assertReleaseVerdict(t, rec.Body.Bytes())
}

func TestRateLimit(t *testing.T) {
	h := newTestHandler(t)
	mustSetScript(t, h, "rate_limit")

	rec := business(t, h, pathResponses)
	assertSSE(t, rec, wantRateLimit)
	assertRateLimitVerdict(t, rec.Body.Bytes())
}

func TestRateLimitFailedOnly(t *testing.T) {
	h := newTestHandler(t)
	mustSetScript(t, h, "rate_limit_failed_only")

	rec := business(t, h, pathResponses)
	assertSSE(t, rec, wantFailedOnly)
	assertRateLimitVerdict(t, rec.Body.Bytes())
}

func TestQuota(t *testing.T) {
	h := newTestHandler(t)
	mustSetScript(t, h, "quota")

	rec := business(t, h, pathResponses)
	assertSSE(t, rec, wantQuota)
	assertReleaseVerdict(t, rec.Body.Bytes())
}

func TestChatRateLimit(t *testing.T) {
	h := newTestHandler(t)
	mustSetScript(t, h, "chat_rate_limit")

	rec := business(t, h, pathChat)
	assertSSE(t, rec, wantChatRateLimit)
	assertRateLimitVerdict(t, rec.Body.Bytes())
}

func TestHTTP429(t *testing.T) {
	h := newTestHandler(t)
	mustSetScript(t, h, "http429")

	for _, path := range []string{pathResponses, pathChat} {
		rec := business(t, h, path)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("%s: status = %d, want 429", path, rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); got != "application/json" {
			t.Fatalf("%s: Content-Type = %q", path, got)
		}
		if got := rec.Body.String(); got != wantHTTP429Body {
			t.Fatalf("%s: body = %q, want %q", path, got, wantHTTP429Body)
		}
	}
}

func TestSlow(t *testing.T) {
	h := newTestHandler(t)
	mustSetScript(t, h, "slow:1")

	start := time.Now()
	rec := business(t, h, pathResponses)
	elapsed := time.Since(start)

	assertSSE(t, rec, wantResponsesSlow1)
	if elapsed < 900*time.Millisecond {
		t.Fatalf("slow:1 returned after %v, the delta was not delayed", elapsed)
	}
	assertReleaseVerdict(t, rec.Body.Bytes())
}

func TestSlowOnChat(t *testing.T) {
	h := newTestHandler(t)
	mustSetScript(t, h, "slow:1")

	const wantBody = "data: " + chatHelloJSON + "\n\n" +
		"data: " + chatStopJSON + "\n\n" +
		"data: [DONE]\n\n"

	start := time.Now()
	rec := business(t, h, pathChat)
	elapsed := time.Since(start)

	assertSSE(t, rec, wantBody)
	if elapsed < 900*time.Millisecond {
		t.Fatalf("slow:1 on chat returned after %v, the chunk was not delayed", elapsed)
	}
}

func TestHold(t *testing.T) {
	h := newTestHandler(t)
	mustSetScript(t, h, "hold:1")

	start := time.Now()
	rec := business(t, h, pathResponses)
	elapsed := time.Since(start)

	assertSSE(t, rec, wantResponsesOK)
	if elapsed < 900*time.Millisecond {
		t.Fatalf("hold:1 returned after %v, the headers were not held", elapsed)
	}
}

func TestHoldOnChat(t *testing.T) {
	h := newTestHandler(t)
	mustSetScript(t, h, "hold:1")

	start := time.Now()
	rec := business(t, h, pathChat)
	elapsed := time.Since(start)

	assertSSE(t, rec, wantChatOK)
	if elapsed < 900*time.Millisecond {
		t.Fatalf("hold:1 on chat returned after %v, the headers were not held", elapsed)
	}
}

func TestHoldRateLimit(t *testing.T) {
	h := newTestHandler(t)
	mustSetScript(t, h, "hold_rate_limit:1")

	rec := business(t, h, pathResponses)
	assertSSE(t, rec, wantRateLimit)
	assertRateLimitVerdict(t, rec.Body.Bytes())
}

func TestStallRateLimit(t *testing.T) {
	h := newTestHandler(t)
	mustSetScript(t, h, "stall_rate_limit:1")

	start := time.Now()
	rec := business(t, h, pathResponses)
	elapsed := time.Since(start)

	assertSSE(t, rec, wantRateLimit)
	if elapsed < 900*time.Millisecond {
		t.Fatalf("stall_rate_limit:1 returned after %v, the frames were not stalled", elapsed)
	}
	assertRateLimitVerdict(t, rec.Body.Bytes())

	// The status and the headers are committed before the stall: with an
	// already-cancelled client nothing is written after them.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	rec = businessWithContext(t, h, pathResponses, cancelled)
	if rec.Code != http.StatusOK || !rec.Flushed || rec.Body.Len() != 0 {
		t.Fatalf("stall: status = %d, flushed = %v, body = %q; want an early 200 and no frames",
			rec.Code, rec.Flushed, rec.Body.String())
	}
}

func TestHoldHTTP429(t *testing.T) {
	h := newTestHandler(t)
	mustSetScript(t, h, "hold_http429:1")

	start := time.Now()
	rec := business(t, h, pathChat)
	elapsed := time.Since(start)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := rec.Body.String(); got != wantHTTP429Body {
		t.Fatalf("body = %q, want %q", got, wantHTTP429Body)
	}
	if elapsed < 900*time.Millisecond {
		t.Fatalf("hold_http429:1 returned after %v, the headers were not held", elapsed)
	}
}

func TestHoldHTTP429OnResponses(t *testing.T) {
	h := newTestHandler(t)
	mustSetScript(t, h, "hold_http429:1")

	start := time.Now()
	rec := business(t, h, pathResponses)
	elapsed := time.Since(start)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := rec.Body.String(); got != wantHTTP429Body {
		t.Fatalf("body = %q, want %q", got, wantHTTP429Body)
	}
	if elapsed < 900*time.Millisecond {
		t.Fatalf("hold_http429:1 on responses returned after %v, the headers were not held", elapsed)
	}
}

// TestCancelledClientStopsDelays checks the requirement that no delay runs past
// the client: every item that waits must return at once on ctx.Done(), which is
// only possible because the waits use a timer plus select.
func TestCancelledClientStopsDelays(t *testing.T) {
	tests := []struct {
		item     string
		path     string
		wantBody string // "" means nothing may be written
	}{
		{item: "hold:1", path: pathResponses, wantBody: ""},
		{item: "hold_rate_limit:1", path: pathResponses, wantBody: ""},
		{item: "hold_http429:1", path: pathChat, wantBody: ""},
		{item: "slow:1", path: pathResponses, wantBody: "event: response.created\ndata: " + createdJSON + "\n\n"},
	}

	for _, tt := range tests {
		t.Run(tt.item, func(t *testing.T) {
			h := newTestHandler(t)
			mustSetScript(t, h, tt.item)

			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			start := time.Now()
			rec := businessWithContext(t, h, tt.path, ctx)
			if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
				t.Fatalf("%s ignored the cancelled client: returned after %v", tt.item, elapsed)
			}
			if got := rec.Body.String(); got != tt.wantBody {
				t.Fatalf("%s body = %q, want %q", tt.item, got, tt.wantBody)
			}
		})
	}
}

// TestClosedConnectionStopsHold uses a real TCP connection and a request body,
// as NewAPI sends: net/http notices a closed client only after the handler has
// read the body, so a hold must still end soon after the connection is gone.
// A cancelled context alone (above) does not exercise that path.
func TestClosedConnectionStopsHold(t *testing.T) {
	h := newTestHandler(t)
	mustSetScript(t, h, "hold:5")

	arrived := make(chan struct{})
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		close(arrived)
		h.ServeHTTP(w, r)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+pathResponses,
		strings.NewReader(`{"model":"gpt-6-astra","stream":true,"input":"hi"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	errc := make(chan error, 1)
	go func() {
		resp, err := srv.Client().Do(req)
		if err == nil {
			resp.Body.Close()
		}
		errc <- err
	}()

	<-arrived
	cancel() // the client transport closes the connection
	if err := <-errc; err == nil {
		t.Fatal("request finished although the client was cancelled during hold:5")
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("hold:5 kept waiting after the client closed its connection")
	}
}

// --- /__mode and /__log ----------------------------------------------------

func TestModeEchoesAcceptedScript(t *testing.T) {
	h := newTestHandler(t)
	rec := setScript(t, h, "ok", "slow:2")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Body.String(); got != `{"script":["ok","slow:2"]}` {
		t.Fatalf("body = %q", got)
	}
}

func TestModeRejectsInvalidScript(t *testing.T) {
	bodies := []string{
		`{}`,
		`{"script":[]}`,
		`{"script":["bogus"]}`,
		`{"script":["ok","bogus"]}`,
		`{"script":["slow"]}`,
		`{"script":["slow:0"]}`,
		`{"script":["slow:-1"]}`,
		`{"script":["slow:abc"]}`,
		`{"script":["slow:1.5"]}`,
		`{"script":["hold:0"]}`,
		`{"script":["ok:1"]}`,
		`{"script":`,
		`not json at all`,
	}

	for _, body := range bodies {
		t.Run(body, func(t *testing.T) {
			h := newTestHandler(t)
			mustSetScript(t, h, "rate_limit_failed_only")

			// One business request so the old script and the log are in a known state.
			if rec := business(t, h, pathResponses); rec.Body.String() != wantFailedOnly {
				t.Fatalf("setup request body = %q", rec.Body.String())
			}

			req := httptest.NewRequest(http.MethodPost, "/__mode", strings.NewReader(body))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}

			// The old script survives and the log/count are untouched.
			if got := business(t, h, pathResponses).Body.String(); got != wantFailedOnly {
				t.Fatalf("old script was replaced: body = %q", got)
			}
			if got := readLog(t, h); got.Count != 2 {
				t.Fatalf("rejected /__mode cleared the log: count = %d, want 2", got.Count)
			}
		})
	}
}

func TestModeResetsCountAndLog(t *testing.T) {
	h := newTestHandler(t)
	mustSetScript(t, h, "ok")
	business(t, h, pathResponses)
	business(t, h, pathResponses)
	if got := readLog(t, h); got.Count != 2 {
		t.Fatalf("count before reset = %d, want 2", got.Count)
	}

	mustSetScript(t, h, "rate_limit_failed_only")

	if got := readLog(t, h); got.Count != 0 || len(got.Requests) != 0 {
		t.Fatalf("log after reset = %+v, want empty", got)
	}
	if got := business(t, h, pathResponses).Body.String(); got != wantFailedOnly {
		t.Fatalf("new script not in effect: body = %q", got)
	}
	log := readLog(t, h)
	if log.Count != 1 || log.Requests[0].Seq != 1 || log.Requests[0].Item != "rate_limit_failed_only" {
		t.Fatalf("log after reset = %+v", log)
	}
}

func TestScriptOrderAndRepeat(t *testing.T) {
	h := newTestHandler(t)
	mustSetScript(t, h, "rate_limit", "rate_limit_failed_only")

	wantBodies := []string{wantRateLimit, wantFailedOnly, wantFailedOnly, wantFailedOnly}
	for i, want := range wantBodies {
		if got := business(t, h, pathResponses).Body.String(); got != want {
			t.Fatalf("request %d: body = %q, want %q", i+1, got, want)
		}
	}

	log := readLog(t, h)
	if log.Count != len(wantBodies) {
		t.Fatalf("count = %d, want %d", log.Count, len(wantBodies))
	}
	wantItems := []string{"rate_limit", "rate_limit_failed_only", "rate_limit_failed_only", "rate_limit_failed_only"}
	for i, entry := range log.Requests {
		if entry.Seq != i+1 {
			t.Fatalf("row %d: seq = %d", i, entry.Seq)
		}
		if entry.Path != pathResponses || entry.Item != wantItems[i] {
			t.Fatalf("row %d = %+v", i, entry)
		}
		if entry.AtUnixMS <= 0 {
			t.Fatalf("row %d: at_unix_ms = %d", i, entry.AtUnixMS)
		}
	}
}

func TestPathMismatch(t *testing.T) {
	h := newTestHandler(t)
	mustSetScript(t, h, "rate_limit")

	rec := business(t, h, pathChat)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got := rec.Body.String(); got != wantMismatchBody {
		t.Fatalf("body = %q, want %q", got, wantMismatchBody)
	}

	// The rejected request is still in the log.
	log := readLog(t, h)
	if log.Count != 1 {
		t.Fatalf("count = %d, want 1", log.Count)
	}
	if got := log.Requests[0]; got.Path != pathChat || got.Item != "rate_limit" || got.Seq != 1 {
		t.Fatalf("log row = %+v", got)
	}

	// The other direction: a chat-only item on the Responses path.
	h2 := newTestHandler(t)
	mustSetScript(t, h2, "chat_rate_limit")
	if rec := business(t, h2, pathResponses); rec.Code != http.StatusBadRequest {
		t.Fatalf("chat_rate_limit on /v1/responses: status = %d, want 400", rec.Code)
	}
}

func TestConcurrentDispatch(t *testing.T) {
	h := newTestHandler(t)
	items := make([]string, 16)
	for i := range items {
		items[i] = "ok"
	}
	mustSetScript(t, h, items...)

	const requests = 16
	bodies := make([]string, requests)
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			bodies[i] = business(t, h, pathResponses).Body.String()
		}(i)
	}
	wg.Wait()

	for i, body := range bodies {
		if body != wantResponsesOK {
			t.Fatalf("request %d: body = %q", i, body)
		}
	}

	log := readLog(t, h)
	if log.Count != requests {
		t.Fatalf("count = %d, want %d", log.Count, requests)
	}
	seen := make(map[int]bool, requests)
	for _, entry := range log.Requests {
		if seen[entry.Seq] {
			t.Fatalf("duplicate seq %d", entry.Seq)
		}
		seen[entry.Seq] = true
	}
	seqs := make([]int, 0, requests)
	for seq := range seen {
		seqs = append(seqs, seq)
	}
	sort.Ints(seqs)
	for i, seq := range seqs {
		if seq != i+1 {
			t.Fatalf("seq set = %v, want 1..%d", seqs, requests)
		}
	}
}

func TestSameItemTwiceIsByteIdentical(t *testing.T) {
	h := newTestHandler(t)
	mustSetScript(t, h, "rate_limit")

	first := business(t, h, pathResponses).Body.String()
	second := business(t, h, pathResponses).Body.String()
	if first != second {
		t.Fatalf("two runs of rate_limit differ:\n first %q\nsecond %q", first, second)
	}
	if first != wantRateLimit {
		t.Fatalf("body = %q, want %q", first, wantRateLimit)
	}
}

func TestNoScriptConfigured(t *testing.T) {
	h := newTestHandler(t)

	rec := business(t, h, pathResponses)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if got := rec.Body.String(); got != `{"error":{"message":"mockrelay: no script configured"}}` {
		t.Fatalf("body = %q", got)
	}
	if log := readLog(t, h); log.Count != 1 || log.Requests[0].Item != "" {
		t.Fatalf("log = %+v, want one row with an empty item", log)
	}
}
