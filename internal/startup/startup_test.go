package startup

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
)

// ltMessage is the LT upstream rate-limit text (渠道监控/429-vs-quota.md).
const ltMessage = "Rate limit reached for gpt-6-astra in organization org-9xKq2LmZ on tokens per min (TPM): " +
	"Limit 40000000, Used 40000000, Requested 19224. Please try again in 28ms."

// sse renders one Server-Sent Event frame.
func sse(name, data string) string {
	if name == "" {
		return "data: " + data + "\n\n"
	}
	return "event: " + name + "\ndata: " + data + "\n\n"
}

// drain returns whatever Rest still holds. A sticky read error is expected for
// the failing readers below, so the error itself is ignored.
func drain(r *bufio.Reader) []byte {
	if r == nil {
		return nil
	}
	b, _ := io.ReadAll(r)
	return b
}

// failingReader serves data once and then fails forever.
type failingReader struct {
	data []byte
	err  error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	return 0, r.err
}

func TestConstants(t *testing.T) {
	if MaxBytes != 256<<10 {
		t.Fatalf("MaxBytes = %d, want 256<<10", MaxBytes)
	}
	if MaxEvents != 128 {
		t.Fatalf("MaxEvents = %d, want 128", MaxEvents)
	}
}

func TestReadSamples(t *testing.T) {
	created := sse("response.created", `{"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`)
	ltError := sse("error", `{"type":"error","error":{"type":"tokens","code":"rate_limit_exceeded","message":"`+ltMessage+`"}}`)
	done := sse("", "[DONE]")

	var startup128 strings.Builder
	for i := 0; i < MaxEvents; i++ {
		startup128.WriteString(sse("response.created", `{"type":"response.created"}`))
	}
	var startup129 strings.Builder
	for i := 0; i <= MaxEvents; i++ {
		startup129.WriteString(sse("response.created", `{"type":"response.created"}`))
	}

	tests := []struct {
		name      string
		stream    string
		verdict   Verdict
		checkRest bool
		wantRest  string
		wantErr   *UpstreamError
	}{
		{
			name:      "LT rate-limit error frame, trailing bytes stay in Rest",
			stream:    created + ltError + done,
			verdict:   VerdictRateLimit,
			checkRest: true,
			wantRest:  done,
			wantErr: &UpstreamError{
				Message: ltMessage,
				Type:    "tokens",
				Code:    []byte(`"rate_limit_exceeded"`),
			},
		},
		{
			name:    "response.failed carries the error in response.error",
			stream:  sse("response.failed", `{"type":"response.failed","response":{"id":"resp_2","status":"failed","error":{"type":"tokens","code":"rate_limit_exceeded","message":"`+ltMessage+`"}}}`),
			verdict: VerdictRateLimit,
			wantErr: &UpstreamError{
				Message: ltMessage,
				Type:    "tokens",
				Code:    []byte(`"rate_limit_exceeded"`),
			},
		},
		{
			name:    "Chat Completions error chunk has no type at all",
			stream:  sse("", `{"error":{"type":"tokens","code":"rate_limit_exceeded","message":"`+ltMessage+`"}}`),
			verdict: VerdictRateLimit,
			wantErr: &UpstreamError{
				Message: ltMessage,
				Type:    "tokens",
				Code:    []byte(`"rate_limit_exceeded"`),
			},
		},
		{
			name:    "top-level error frame uses only its own code and message, like H3",
			stream:  sse("error", `{"type":"error","code":"rate_limit_exceeded","message":"top-level rate limit","param":"x"}`),
			verdict: VerdictRateLimit,
			wantErr: &UpstreamError{
				Message: "top-level rate limit",
				Code:    []byte(`"rate_limit_exceeded"`),
			},
		},
		{
			name:    "response.failed with only top-level fields falls back like H3",
			stream:  created + sse("response.failed", `{"type":"response.failed","code":"rate_limit_exceeded","message":"`+"Rate limit reached for gpt-6-astra"+`"}`),
			verdict: VerdictRateLimit,
			wantErr: &UpstreamError{
				Message: "Rate limit reached for gpt-6-astra",
				Code:    []byte(`"rate_limit_exceeded"`),
			},
		},
		{
			name:    "a non-string response.status does not break the frame",
			stream:  sse("response.created", `{"type":"response.created","response":{"status":1}}`) + sse("response.output_text.delta", `{"type":"response.output_text.delta","delta":"hi"}`),
			verdict: VerdictRelease,
		},
		{
			name:    "comment frames and delta stay in the prefix",
			stream:  ": PING\n\n" + created + sse("response.output_text.delta", `{"type":"response.output_text.delta","delta":"hi"}`),
			verdict: VerdictRelease,
		},
		{
			name:    "single line over MaxBytes releases with the whole stream",
			stream:  created + ": " + strings.Repeat("x", MaxBytes) + "\n",
			verdict: VerdictRelease,
		},
		{
			name:    "exactly MaxEvents startup events release at EOF",
			stream:  startup128.String(),
			verdict: VerdictRelease,
		},
		{
			name:      "one event over MaxEvents releases with everything read",
			stream:    startup129.String(),
			verdict:   VerdictRelease,
			checkRest: true,
			wantRest:  "",
		},
		{
			name:    "invalid JSON releases instead of failing",
			stream:  created + sse("", "{not json"),
			verdict: VerdictRelease,
		},
		{
			name:    "[DONE] releases",
			stream:  created + done,
			verdict: VerdictRelease,
		},
		{
			name:    "empty stream releases with an empty prefix",
			stream:  "",
			verdict: VerdictRelease,
		},
		{
			name:    "insufficient_quota is not a rate limit",
			stream:  sse("error", `{"type":"error","error":{"type":"insufficient_quota","code":"insufficient_quota","message":"You exceeded your current quota"}}`),
			verdict: VerdictRelease,
		},
		{
			name:    "first event is already a normal delta",
			stream:  sse("", `{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"hi"}}]}`),
			verdict: VerdictRelease,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := Read(strings.NewReader(tt.stream))
			rest := drain(res.Rest)

			if got := string(res.Prefix) + string(rest); got != tt.stream {
				t.Fatalf("Prefix+Rest != original stream:\n got %q\nwant %q", got, tt.stream)
			}
			if res.Verdict != tt.verdict {
				t.Fatalf("Verdict = %d, want %d", res.Verdict, tt.verdict)
			}
			if tt.checkRest && string(rest) != tt.wantRest {
				t.Fatalf("Rest = %q, want %q", rest, tt.wantRest)
			}
			if tt.wantErr == nil {
				if res.Err.Message != "" || res.Err.Type != "" || res.Err.Param != "" || len(res.Err.Code) != 0 {
					t.Fatalf("Err = %+v, want zero", res.Err)
				}
				return
			}
			if res.Err.Message != tt.wantErr.Message {
				t.Fatalf("Err.Message = %q, want %q", res.Err.Message, tt.wantErr.Message)
			}
			if res.Err.Type != tt.wantErr.Type {
				t.Fatalf("Err.Type = %q, want %q", res.Err.Type, tt.wantErr.Type)
			}
			if res.Err.Param != tt.wantErr.Param {
				t.Fatalf("Err.Param = %q, want %q", res.Err.Param, tt.wantErr.Param)
			}
			if string(res.Err.Code) != string(tt.wantErr.Code) {
				t.Fatalf("Err.Code = %q, want %q", res.Err.Code, tt.wantErr.Code)
			}
		})
	}
}

func TestReadKeepsUnterminatedFinalEvent(t *testing.T) {
	// A stream that ends in the middle of an event, then fails: the partial
	// bytes must come back in Prefix, otherwise Prefix+Rest would lose them.
	partial := `data: {"type":"response.created"}`
	res := Read(&failingReader{data: []byte(partial), err: errors.New("connection reset")})

	if res.Verdict != VerdictRelease {
		t.Fatalf("Verdict = %d, want release", res.Verdict)
	}
	if got := string(res.Prefix); got != partial {
		t.Fatalf("Prefix = %q, want %q", got, partial)
	}
	if rest := drain(res.Rest); len(rest) != 0 {
		t.Fatalf("Rest = %q, want empty", rest)
	}
}

func TestReadHandlesReadErrorAfterCompleteEvent(t *testing.T) {
	first := sse("response.created", `{"type":"response.created"}`)
	res := Read(&failingReader{data: []byte(first), err: errors.New("connection reset")})

	if res.Verdict != VerdictRelease {
		t.Fatalf("Verdict = %d, want release", res.Verdict)
	}
	if got := string(res.Prefix); got != first {
		t.Fatalf("Prefix = %q, want %q", got, first)
	}
}

// endlessLine yields n bytes of 'x' and never a newline, then EOF. It counts
// how much Read pulled from it.
type endlessLine struct {
	n, served int
}

func (r *endlessLine) Read(p []byte) (int, error) {
	if r.served >= r.n {
		return 0, io.EOF
	}
	k := min(len(p), r.n-r.served)
	for i := range k {
		p[i] = 'x'
	}
	r.served += k
	return k, nil
}

// A huge line without a newline must be cut at the byte budget instead of being
// buffered whole (design §1.3: readLine), and nothing read may be lost.
func TestReadBoundsAnOverlongLine(t *testing.T) {
	src := &endlessLine{n: 64 << 20}
	res := Read(src)

	if res.Verdict != VerdictRelease {
		t.Fatalf("Verdict = %d, want release", res.Verdict)
	}
	// One bufio.Reader fill (4096 bytes by default) may overshoot the budget.
	if len(res.Prefix) > MaxBytes+4096 {
		t.Fatalf("Prefix holds %d bytes, want at most MaxBytes plus one buffer", len(res.Prefix))
	}
	if src.served > MaxBytes+2*4096 {
		t.Fatalf("Read pulled %d bytes from the upstream before giving up", src.served)
	}
	rest := drain(res.Rest)
	if len(res.Prefix)+len(rest) != src.n {
		t.Fatalf("Prefix+Rest = %d bytes, want %d", len(res.Prefix)+len(rest), src.n)
	}
}

func TestReadMultiLineData(t *testing.T) {
	// The data of one event may arrive on several data: lines; they are joined
	// with a newline before the JSON parse.
	stream := "event: error\ndata: {\"type\":\"error\",\ndata: \"code\":\"rate_limit_exceeded\",\ndata: \"message\":\"split\"}\n\n"
	res := Read(strings.NewReader(stream))

	if res.Verdict != VerdictRateLimit {
		t.Fatalf("Verdict = %d, want rate limit", res.Verdict)
	}
	if res.Err.Code == nil || string(res.Err.Code) != `"rate_limit_exceeded"` {
		t.Fatalf("Err.Code = %q", res.Err.Code)
	}
	if res.Err.Message != "split" {
		t.Fatalf("Err.Message = %q, want split", res.Err.Message)
	}
}

// TestReadHeartbeatFlags covers the two fields that let the proxy recognise
// NewAPI's terminal error after a heartbeat (design §1.3, §1.6).
func TestReadHeartbeatFlags(t *testing.T) {
	ping := ": PING\n\n"
	untyped := sse("", `{"error":{"type":"tokens","code":"rate_limit_exceeded","message":"`+ltMessage+`"}}`)
	typedByEvent := sse("error", `{"type":"error","error":{"type":"tokens","code":"rate_limit_exceeded","message":"`+ltMessage+`"}}`)
	typedByField := sse("", `{"type":"error","code":"rate_limit_exceeded","message":"`+ltMessage+`"}`)
	done := sse("", "[DONE]")
	created := sse("response.created", `{"type":"response.created"}`)
	eventOnly := "event: ping\n\n"

	tests := []struct {
		name          string
		stream        string
		heartbeatOnly bool
		untyped       bool
		checkRest     bool
		wantRest      string
	}{
		{
			name:          "two heartbeats then an untyped rate limit then [DONE]",
			stream:        ping + ping + untyped + done,
			heartbeatOnly: true,
			untyped:       true,
			checkRest:     true,
			wantRest:      done,
		},
		{
			name:    "no heartbeat, the first frame is already the untyped rate limit",
			stream:  untyped + done,
			untyped: true,
		},
		{
			name:   "heartbeat then response.created then a typed rate limit",
			stream: ping + created + typedByEvent,
		},
		{
			name:          "heartbeat then a typed error frame",
			stream:        ping + typedByEvent,
			heartbeatOnly: true,
		},
		{
			name:          "heartbeat then a frame that carries a type field",
			stream:        ping + typedByField,
			heartbeatOnly: true,
		},
		{
			name:    "an event with only an event: line is not a heartbeat",
			stream:  ping + eventOnly + untyped + done,
			untyped: true,
		},
		{
			name:    "a blank line on its own is not a heartbeat",
			stream:  "\n" + untyped + done,
			untyped: true,
		},
		{
			name:          "a blank line next to a heartbeat does not hide it",
			stream:        "\n" + ping + untyped + done,
			heartbeatOnly: true,
			untyped:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := Read(strings.NewReader(tt.stream))
			rest := drain(res.Rest)

			if got := string(res.Prefix) + string(rest); got != tt.stream {
				t.Fatalf("Prefix+Rest != original stream:\n got %q\nwant %q", got, tt.stream)
			}
			if res.Verdict != VerdictRateLimit {
				t.Fatalf("Verdict = %d, want rate limit", res.Verdict)
			}
			if res.HeartbeatOnly != tt.heartbeatOnly {
				t.Fatalf("HeartbeatOnly = %v, want %v", res.HeartbeatOnly, tt.heartbeatOnly)
			}
			if res.Untyped != tt.untyped {
				t.Fatalf("Untyped = %v, want %v", res.Untyped, tt.untyped)
			}
			if tt.checkRest && string(rest) != tt.wantRest {
				t.Fatalf("Rest = %q, want %q", rest, tt.wantRest)
			}
		})
	}
}

// TestReadRawJSONAfterHeartbeatIsRateLimit is rc.45's terminal error after a
// heartbeat: Relay's defer writes the error object with c.JSON, so it arrives
// without a data: line and without a trailing newline, and only the raw JSON
// rule can recognise it (design §1.3, measured by T10 T-F17).
func TestReadRawJSONAfterHeartbeatIsRateLimit(t *testing.T) {
	raw := `{"error":{"type":"tokens","code":"rate_limit_exceeded","message":"` + ltMessage + `"}}`
	stream := ": PING\n\n: PING\n\n" + raw
	res := Read(strings.NewReader(stream))

	if res.Verdict != VerdictRateLimit {
		t.Fatalf("Verdict = %d, want rate limit", res.Verdict)
	}
	if !res.HeartbeatOnly || !res.Untyped {
		t.Fatalf("HeartbeatOnly = %v, Untyped = %v, want both true", res.HeartbeatOnly, res.Untyped)
	}
	if res.Err.Message != ltMessage {
		t.Fatalf("Err.Message = %q, want the upstream text", res.Err.Message)
	}
	if got, want := string(res.Err.Code), `"rate_limit_exceeded"`; got != want {
		t.Fatalf("Err.Code = %s, want %s", got, want)
	}
	if got := string(res.Prefix) + string(drain(res.Rest)); got != stream {
		t.Fatalf("Prefix+Rest != original stream:\n got %q\nwant %q", got, stream)
	}
}

// TestReadRawJSONRule covers every condition of the rule one by one: the line
// must follow heartbeats only, must be the only such line of its event, must not
// sit next to an event: or data: line, and must start with '{'. Relaxing any of
// them would let a stray non-field line of an upstream stream become an error
// frame (design §1.3, §8).
func TestReadRawJSONRule(t *testing.T) {
	ping := ": PING\n\n"
	rawRateLimit := `{"error":{"type":"tokens","code":"rate_limit_exceeded","message":"` + ltMessage + `"}}`
	rawOtherError := `{"error":{"type":"bad_response_status_code","code":"bad_response_status_code","message":"upstream said no"}}`
	created := sse("response.created", `{"type":"response.created"}`)
	typedRateLimit := sse("error", `{"type":"error","error":{"type":"tokens","code":"rate_limit_exceeded","message":"`+ltMessage+`"}}`)

	tests := []struct {
		name          string
		stream        string
		verdict       Verdict
		heartbeatOnly bool
		untyped       bool
	}{
		{
			name:          "heartbeats then the raw JSON, no trailing newline",
			stream:        ping + ping + rawRateLimit,
			verdict:       VerdictRateLimit,
			heartbeatOnly: true,
			untyped:       true,
		},
		{
			name:          "heartbeats then the raw JSON and one newline",
			stream:        ping + ping + rawRateLimit + "\n",
			verdict:       VerdictRateLimit,
			heartbeatOnly: true,
			untyped:       true,
		},
		{
			name:          "heartbeats then the raw JSON and a blank line",
			stream:        ping + ping + rawRateLimit + "\n\n",
			verdict:       VerdictRateLimit,
			heartbeatOnly: true,
			untyped:       true,
		},
		{
			name:    "the raw JSON is not a rate limit",
			stream:  ping + rawOtherError,
			verdict: VerdictRelease,
		},
		{
			name:    "the raw JSON comes without a heartbeat",
			stream:  rawRateLimit,
			verdict: VerdictRelease,
		},
		{
			name:    "an upstream startup event came before the raw JSON",
			stream:  ping + created + rawRateLimit,
			verdict: VerdictRelease,
		},
		{
			name:    "two raw lines in the same event",
			stream:  ping + rawRateLimit + "\n" + `{"a":1}` + "\n\n",
			verdict: VerdictRelease,
		},
		{
			// The rate limit is the last raw line here, so only the rawLines == 1
			// condition keeps it from being read as the deciding frame.
			name:    "two raw lines in the same event, the rate limit last",
			stream:  ping + `{"a":1}` + "\n" + rawRateLimit + "\n\n",
			verdict: VerdictRelease,
		},
		{
			name:    "the raw JSON sits next to an event: line",
			stream:  ping + "event: response.ping\n" + rawRateLimit + "\n\n",
			verdict: VerdictRelease,
		},
		{
			name:    "the raw JSON sits next to a data: line",
			stream:  ping + "data:\n" + rawRateLimit + "\n\n",
			verdict: VerdictRelease,
		},
		{
			name:    "a non-JSON line after a heartbeat",
			stream:  ping + "hello\n\n",
			verdict: VerdictRelease,
		},
		{
			// A line that does not start with '{' is not taken as data: it counts
			// as upstream data like before T16, so the typed frame after it is
			// still read and intercepted, and the heartbeat-only flag is off.
			name:    "a non-JSON line after a heartbeat, then a typed rate limit",
			stream:  ping + "hello\n\n" + typedRateLimit,
			verdict: VerdictRateLimit,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := Read(strings.NewReader(tt.stream))
			if got := string(res.Prefix) + string(drain(res.Rest)); got != tt.stream {
				t.Fatalf("Prefix+Rest != original stream:\n got %q\nwant %q", got, tt.stream)
			}
			if res.Verdict != tt.verdict {
				t.Fatalf("Verdict = %d, want %d", res.Verdict, tt.verdict)
			}
			if res.Verdict != VerdictRateLimit {
				return
			}
			if res.HeartbeatOnly != tt.heartbeatOnly || res.Untyped != tt.untyped {
				t.Fatalf("HeartbeatOnly/Untyped = %v/%v, want %v/%v",
					res.HeartbeatOnly, res.Untyped, tt.heartbeatOnly, tt.untyped)
			}
		})
	}
}
