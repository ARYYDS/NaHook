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
			name:    "top-level error frame uses its own code and message",
			stream:  sse("error", `{"type":"error","code":"rate_limit_exceeded","message":"top-level rate limit"}`),
			verdict: VerdictRateLimit,
			wantErr: &UpstreamError{
				Message: "top-level rate limit",
				Type:    "error",
				Code:    []byte(`"rate_limit_exceeded"`),
			},
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
