package proxy429

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ARYYDS/NaHook/internal/startup"
)

// ltMessage is the LT upstream text (渠道监控/429-vs-quota.md). It contains no
// character that encoding/json would escape, so it can be pasted into a golden
// body as is.
const ltMessage = "Rate limit reached for gpt-6-astra in organization org-9xKq2LmZ on tokens per min (TPM): " +
	"Limit 40000000, Used 40000000, Requested 19224. Please try again in 28ms."

func TestWriteGolden(t *testing.T) {
	tests := []struct {
		name      string
		requestID string
		upstream  startup.UpstreamError
		wantBody  string
	}{
		{
			name:      "string code, LT message, empty param",
			requestID: "req-lt-1",
			upstream: startup.UpstreamError{
				Message: ltMessage,
				Type:    "tokens",
				Param:   "",
				Code:    json.RawMessage(`"rate_limit_exceeded"`),
			},
			wantBody: `{"error":{"message":"` + ltMessage + `","type":"tokens","param":"","code":"rate_limit_exceeded"}}`,
		},
		{
			name:      "numeric code",
			requestID: "",
			upstream: startup.UpstreamError{
				Message: "rate limited",
				Type:    "tokens",
				Code:    json.RawMessage(`429`),
			},
			wantBody: `{"error":{"message":"rate limited","type":"tokens","param":"","code":429}}`,
		},
		{
			name:      "null code",
			requestID: "req-null",
			upstream: startup.UpstreamError{
				Message: "rate limited",
				Type:    "tokens",
			},
			wantBody: `{"error":{"message":"rate limited","type":"tokens","param":"","code":null}}`,
		},
		{
			name:      "param carries the upstream value",
			requestID: "",
			upstream: startup.UpstreamError{
				Message: "bad request",
				Type:    "invalid_request_error",
				Param:   "temperature",
				Code:    json.RawMessage(`"invalid_request_error"`),
			},
			wantBody: `{"error":{"message":"bad request","type":"invalid_request_error","param":"temperature","code":"invalid_request_error"}}`,
		},
		{
			name:      "HTML characters use the encoding/json default escaping",
			requestID: "",
			upstream: startup.UpstreamError{
				Message: `Rate <b>limit</b> & "quota" reached`,
				Type:    "tokens",
				Code:    json.RawMessage(`"rate_limit_exceeded"`),
			},
			wantBody: `{"error":{"message":"Rate \u003cb\u003elimit\u003c/b\u003e \u0026 \"quota\" reached","type":"tokens","param":"","code":"rate_limit_exceeded"}}`,
		},
		{
			name:      "all fields empty",
			requestID: "",
			upstream:  startup.UpstreamError{},
			wantBody:  `{"error":{"message":"","type":"","param":"","code":null}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			Write(rec, tt.requestID, tt.upstream)

			if rec.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want 429", rec.Code)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
				t.Fatalf("Content-Type = %q", got)
			}
			if got := rec.Header().Get("Retry-After"); got != "" {
				t.Fatalf("Retry-After = %q, the response must not carry one", got)
			}

			if tt.requestID == "" {
				if _, ok := rec.Header()["X-Oneapi-Request-Id"]; ok {
					t.Fatalf("X-Oneapi-Request-Id present for an empty request id")
				}
			} else if got := rec.Header().Get("X-Oneapi-Request-Id"); got != tt.requestID {
				t.Fatalf("X-Oneapi-Request-Id = %q, want %q", got, tt.requestID)
			}

			body := rec.Body.String()
			if body != tt.wantBody {
				t.Fatalf("body:\n got %q\nwant %q", body, tt.wantBody)
			}
			if strings.HasSuffix(body, "\n") {
				t.Fatal("body ends with a newline; json.Marshal must not add one")
			}
			if !json.Valid([]byte(body)) {
				t.Fatalf("body is not valid JSON: %q", body)
			}

			var decoded struct {
				Error struct {
					Message string          `json:"message"`
					Type    string          `json:"type"`
					Param   string          `json:"param"`
					Code    json.RawMessage `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(body), &decoded); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if decoded.Error.Message != tt.upstream.Message {
				t.Fatalf("message was rewritten: %q", decoded.Error.Message)
			}
			if decoded.Error.Type != tt.upstream.Type || decoded.Error.Param != tt.upstream.Param {
				t.Fatalf("type/param = %q/%q, want %q/%q",
					decoded.Error.Type, decoded.Error.Param, tt.upstream.Type, tt.upstream.Param)
			}
			wantCode := "null"
			if tt.upstream.Code != nil {
				wantCode = string(tt.upstream.Code)
			}
			if string(decoded.Error.Code) != wantCode {
				t.Fatalf("code = %q, want %q (passed through unchanged)", decoded.Error.Code, wantCode)
			}
		})
	}
}

// TestWriteFieldOrder pins the wire order, which the golden bodies also cover:
// a struct reorder must not slip through as "still valid JSON".
func TestWriteFieldOrder(t *testing.T) {
	rec := httptest.NewRecorder()
	Write(rec, "", startup.UpstreamError{
		Message: "m",
		Type:    "t",
		Param:   "p",
		Code:    json.RawMessage(`"c"`),
	})

	body := rec.Body.String()
	want := `{"error":{"message":"m","type":"t","param":"p","code":"c"}}`
	if body != want {
		t.Fatalf("body = %q, want %q", body, want)
	}
	if i, j := strings.Index(body, `"message"`), strings.Index(body, `"type"`); i > j {
		t.Fatal("message must come before type")
	}
}

// TestWriteInvalidCodeDoesNotPanic keeps a caller bug from taking the request
// down: startup only emits canonical JSON, so this path is unreachable in
// production.
func TestWriteInvalidCodeDoesNotPanic(t *testing.T) {
	rec := httptest.NewRecorder()
	Write(rec, "", startup.UpstreamError{Message: "m", Code: json.RawMessage(`{"bad`)})

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
}
