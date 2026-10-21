package startup

import (
	"encoding/json"
	"strings"
	"testing"
)

func frameOf(t *testing.T, raw string) frame {
	t.Helper()
	var f frame
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		t.Fatalf("unmarshal frame %q: %v", raw, err)
	}
	return f
}

func TestUpstreamErrorOrder(t *testing.T) {
	tests := []struct {
		name      string
		eventName string
		frame     string
		ok        bool
		want      UpstreamError
	}{
		{
			name:      "frame.error wins over response.error",
			eventName: "",
			frame:     `{"type":"error","error":{"message":"m1","type":"tokens","param":"p1","code":"rate_limit_exceeded"},"response":{"status":"failed","error":{"message":"m2"}}}`,
			ok:        true,
			want:      UpstreamError{Message: "m1", Type: "tokens", Param: "p1", Code: json.RawMessage(`"rate_limit_exceeded"`)},
		},
		{
			name:      "null frame.error falls through to response.error",
			eventName: "",
			frame:     `{"type":"response.failed","error":null,"response":{"status":"failed","error":{"message":"m2","type":"tokens","code":429}}}`,
			ok:        true,
			want:      UpstreamError{Message: "m2", Type: "tokens", Code: json.RawMessage(`429`)},
		},
		{
			name:      "response.error may be a bare string",
			eventName: "",
			frame:     `{"type":"response.failed","response":{"error":"boom"}}`,
			ok:        true,
			want:      UpstreamError{Message: "boom", Type: "error"},
		},
		{
			name:      "top-level error event uses only code and message, like H3",
			eventName: "",
			frame:     `{"type":"error","code":"rate_limit_exceeded","message":"m3"}`,
			ok:        true,
			want:      UpstreamError{Message: "m3", Code: json.RawMessage(`"rate_limit_exceeded"`)},
		},
		{
			name:      "event name supplies the type when the frame has none",
			eventName: "error",
			frame:     `{"code":"rate_limit_exceeded","message":"m4"}`,
			ok:        true,
			want:      UpstreamError{Message: "m4", Code: json.RawMessage(`"rate_limit_exceeded"`)},
		},
		{
			name:      "top-level param is not carried, like H3",
			eventName: "error",
			frame:     `{"code":"invalid_request_error","message":"m5","param":"temperature"}`,
			ok:        true,
			want:      UpstreamError{Message: "m5", Code: json.RawMessage(`"invalid_request_error"`)},
		},
		{
			name:      "response.failed without an error object falls back to its own fields",
			eventName: "",
			frame:     `{"type":"response.failed","code":"rate_limit_exceeded","message":"m6"}`,
			ok:        true,
			want:      UpstreamError{Message: "m6", Code: json.RawMessage(`"rate_limit_exceeded"`)},
		},
		{
			name:      "response.error event falls back too",
			eventName: "response.error",
			frame:     `{"code":429}`,
			ok:        true,
			want:      UpstreamError{Message: failureMessage, Code: json.RawMessage(`429`)},
		},
		{
			name:      "status failed on any event falls back",
			eventName: "",
			frame:     `{"type":"response.completed","response":{"status":"FAILED"}}`,
			ok:        true,
			want:      UpstreamError{Message: failureMessage},
		},
		{
			name:      "failed event keeps its error object and fills an empty message",
			eventName: "",
			frame:     `{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded"}}}`,
			ok:        true,
			want:      UpstreamError{Message: failureMessage, Code: json.RawMessage(`"rate_limit_exceeded"`)},
		},
		{
			name:      "non-string fields of the error object are dropped, the object still counts",
			eventName: "",
			frame:     `{"error":{"type":5,"message":"m7","param":{"a":1},"code":"rate_limit_exceeded"}}`,
			ok:        true,
			want:      UpstreamError{Message: "m7", Code: json.RawMessage(`"rate_limit_exceeded"`)},
		},
		{
			name:      "a decimal code is written the way H3 writes it",
			eventName: "",
			frame:     `{"error":{"code":429.0,"message":"m8"}}`,
			ok:        true,
			want:      UpstreamError{Message: "m8", Code: json.RawMessage(`429`)},
		},
		{
			name:      "frame.error as a string",
			eventName: "",
			frame:     `{"error":"plain failure"}`,
			ok:        true,
			want:      UpstreamError{Message: "plain failure", Type: "error"},
		},
		{
			name:      "frame.error as a number",
			eventName: "",
			frame:     `{"error":42}`,
			ok:        true,
			want:      UpstreamError{Message: "42", Type: "unknown_error"},
		},
		{
			name:      "empty error object still counts as an error",
			eventName: "",
			frame:     `{"error":{}}`,
			ok:        true,
			want:      UpstreamError{},
		},
		{
			name:      "startup event without error",
			eventName: "",
			frame:     `{"type":"response.created","response":{"status":"in_progress"}}`,
			ok:        false,
		},
		{
			name:      "null error on a startup event",
			eventName: "",
			frame:     `{"type":"response.created","error":null}`,
			ok:        false,
		},
		{
			name:      "null response error on a failed event uses the H3 fallback message",
			eventName: "",
			frame:     `{"type":"response.failed","response":{"status":"failed","error":null}}`,
			ok:        true,
			want:      UpstreamError{Message: failureMessage},
		},
		{
			name:      "null response error on a startup event is no error",
			eventName: "",
			frame:     `{"type":"response.in_progress","response":{"status":"in_progress","error":null}}`,
			ok:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := frameOf(t, tt.frame)
			// Callers pass the resolved event type, exactly as inspect does.
			typ := strings.TrimSpace(f.Type)
			if typ == "" {
				typ = strings.TrimSpace(tt.eventName)
			}
			got, ok := upstreamError(typ, &f)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v (got %+v)", ok, tt.ok, got)
			}
			if !tt.ok {
				return
			}
			if got.Message != tt.want.Message || got.Type != tt.want.Type || got.Param != tt.want.Param {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
			if string(got.Code) != string(tt.want.Code) {
				t.Fatalf("Code = %q, want %q", got.Code, tt.want.Code)
			}
		})
	}
}

// h3Classify429 replicates the first case of H3 classifyResponsesStartupError
// (relay/channel/openai/responses_startup_error.go:247). code is already
// rendered the way fmt.Sprint renders the decoded JSON value.
func h3Classify429(code, typ string) bool {
	c := strings.ToLower(strings.TrimSpace(code))
	text := strings.NewReplacer("-", "_", " ", "_", ":", "_").Replace(c + " " + strings.ToLower(strings.TrimSpace(typ)))
	return c == "429" || containsAny(text,
		"rate_limit_exceeded", "rate_limit_error", "ratelimitreached",
		"too_many_requests", "toomanyrequests", "insufficient_quota")
}

func TestIsRateLimitMatchesH3(t *testing.T) {
	tests := []struct {
		name       string
		code       string // JSON literal; "" leaves the field absent
		typ        string
		message    string
		wantNahook bool
		h3Code     string // what fmt.Sprint would print for the decoded code
		wantH3     bool
	}{
		{
			name: "string code", code: `"rate_limit_exceeded"`, typ: "tokens",
			wantNahook: true, h3Code: "rate_limit_exceeded", wantH3: true,
		},
		{
			name: "numeric code", code: `429`, typ: "",
			wantNahook: true, h3Code: "429", wantH3: true,
		},
		{
			name: "string form of 429", code: `"429"`, typ: "",
			wantNahook: true, h3Code: "429", wantH3: true,
		},
		{
			name: "camel case code", code: `"RateLimitReached"`, typ: "",
			wantNahook: true, h3Code: "RateLimitReached", wantH3: true,
		},
		{
			name: "type rate_limit_error", code: `""`, typ: "rate_limit_error",
			wantNahook: true, h3Code: "", wantH3: true,
		},
		{
			name: "type too_many_requests", code: `""`, typ: "Too_Many_Requests",
			wantNahook: true, h3Code: "", wantH3: true,
		},
		{
			// Deliberate difference: design §1.3 excludes the quota cases.
			name: "insufficient_quota is excluded by nahook only", code: `"insufficient_quota"`, typ: "insufficient_quota",
			wantNahook: false, h3Code: "insufficient_quota", wantH3: true,
		},
		{
			name: "quota_exceeded is not a rate limit", code: `"quota_exceeded"`, typ: "",
			wantNahook: false, h3Code: "quota_exceeded", wantH3: false,
		},
		{
			name: "null code with a rate-limit type", code: `null`, typ: "too_many_requests",
			wantNahook: true, h3Code: "<nil>", wantH3: true,
		},
		{
			// Deliberate difference: interface 2.1 adds the message rule.
			name: "message only", code: ``, typ: "", message: ltMessage,
			wantNahook: true, h3Code: "<nil>", wantH3: false,
		},
		{
			name: "quota message without code", code: ``, typ: "", message: "You exceeded your current quota, please check your plan",
			wantNahook: false, h3Code: "<nil>", wantH3: false,
		},
		{
			name: "server overloaded", code: `"model_at_capacity"`, typ: "server_overloaded",
			wantNahook: false, h3Code: "model_at_capacity", wantH3: false,
		},
		{
			name: "auth failure", code: `"invalid_api_key"`, typ: "authentication_error",
			wantNahook: false, h3Code: "invalid_api_key", wantH3: false,
		},
		{
			name: "empty", code: ``, typ: "",
			wantNahook: false, h3Code: "<nil>", wantH3: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := UpstreamError{Type: tt.typ, Message: tt.message}
			if tt.code != "" {
				e.Code = json.RawMessage(tt.code)
			}
			if got := IsRateLimit(e); got != tt.wantNahook {
				t.Fatalf("IsRateLimit = %v, want %v", got, tt.wantNahook)
			}
			if got := h3Classify429(tt.h3Code, tt.typ); got != tt.wantH3 {
				t.Fatalf("h3 classify 429 branch = %v, want %v (replica is wrong)", got, tt.wantH3)
			}
		})
	}
}

// TestIsRateLimitIgnoresQuota pins the one classification difference the SPEC
// requires, so a later "helpful" edit cannot quietly re-add it.
func TestIsRateLimitIgnoresQuota(t *testing.T) {
	for _, code := range []string{"insufficient_quota", "quota_exceeded"} {
		e := UpstreamError{Code: json.RawMessage(`"` + code + `"`), Type: code}
		if IsRateLimit(e) {
			t.Fatalf("IsRateLimit(%q) = true, want false", code)
		}
	}
}

func TestCodeText(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{raw: `"rate_limit_exceeded"`, want: "rate_limit_exceeded"},
		{raw: `429`, want: "429"},
		{raw: `429.0`, want: "429"},
		{raw: `null`, want: ""},
		{raw: ``, want: ""},
		// fmt.Sprint of the decoded value, as H3 renders it before matching.
		{raw: `{"nested":true}`, want: `map[nested:true]`},
	}
	for _, tt := range tests {
		if got := codeText(json.RawMessage(tt.raw)); got != tt.want {
			t.Fatalf("codeText(%s) = %q, want %q", tt.raw, got, tt.want)
		}
	}
}

// canonical is the byte form H3 answers with: the value decoded and encoded
// again by encoding/json.
func TestCanonical(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{raw: `"rate_limit_exceeded"`, want: `"rate_limit_exceeded"`},
		{raw: `429.0`, want: `429`},
		{raw: ` 429 `, want: `429`},
		// encoding/json escapes HTML characters; the escape is built at run
		// time so the expected bytes cannot be rewritten by an editor.
		{raw: `"<b>"`, want: strings.ReplaceAll(`"#u003cb#u003e"`, "#", `\`)},
		{raw: `{"b":1,"a":2}`, want: `{"a":2,"b":1}`},
		{raw: `null`, want: ``},
		{raw: ``, want: ``},
	}
	for _, tt := range tests {
		if got := string(canonical(json.RawMessage(tt.raw))); got != tt.want {
			t.Fatalf("canonical(%s) = %s, want %s", tt.raw, got, tt.want)
		}
	}
}

func TestIsStartupEvent(t *testing.T) {
	tests := map[string]bool{
		"response.created":           true,
		"response.in_progress":       true,
		"response.queued":            true,
		" RESPONSE.CREATED ":         true,
		"response.completed":         false,
		"response.output_text.delta": false,
		"error":                      false,
		"":                           false,
	}
	for eventType, want := range tests {
		if got := isStartupEvent(eventType); got != want {
			t.Fatalf("isStartupEvent(%q) = %v, want %v", eventType, got, want)
		}
	}
}
