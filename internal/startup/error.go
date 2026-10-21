package startup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// upstreamError extracts the error fields from one frame, in the order
// responsesStartupFrameError uses (responses_startup_error.go:205,
// interface 2.1): frame.error → frame.response.error → the frame's own
// code/message when the event type is "error".
//
// The fields are returned as they are: no status mapping, no message rewrite.
func upstreamError(eventType string, f *frame) (UpstreamError, bool) {
	switch {
	case hasJSON(f.Error):
		return parseErrorValue(f.Error)
	case f.Response != nil && hasJSON(f.Response.Error):
		return parseErrorValue(f.Response.Error)
	case strings.EqualFold(eventType, "error"):
		// The Chat Completions shape: {"type":"error","code":…,"message":…}.
		// H3 keeps only code and message here; Param is carried too because
		// UpstreamError echoes it into the 429 body (design §1.4).
		return UpstreamError{
			Message: f.Message,
			Type:    eventType,
			Param:   stringValue(f.Param),
			Code:    cloneRaw(f.Code),
		}, true
	}
	return UpstreamError{}, false
}

// parseErrorValue maps one JSON error value the way dto.GetOpenAIError does
// (NEWAPI/relaykit/dto/openai_response.go:605): an object contributes
// message/type/param/code, a bare string becomes Type "error", and any other
// JSON value becomes Type "unknown_error".
func parseErrorValue(raw json.RawMessage) (UpstreamError, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return UpstreamError{}, false
	}

	switch trimmed[0] {
	case '{':
		var obj struct {
			Message string          `json:"message"`
			Type    string          `json:"type"`
			Param   json.RawMessage `json:"param"`
			Code    json.RawMessage `json:"code"`
		}
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			return UpstreamError{}, false
		}
		return UpstreamError{
			Message: obj.Message,
			Type:    obj.Type,
			Param:   stringValue(obj.Param),
			Code:    cloneRaw(obj.Code),
		}, true
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return UpstreamError{}, false
		}
		return UpstreamError{Message: s, Type: "error"}, true
	default:
		var v any
		if err := json.Unmarshal(trimmed, &v); err != nil {
			return UpstreamError{}, false
		}
		return UpstreamError{Message: fmt.Sprint(v), Type: "unknown_error"}, true
	}
}

// IsRateLimit reports whether e is an upstream rate limit that is worth a
// same-site retry (design §1.3). It matches the 429 case of H3
// classifyResponsesStartupError (responses_startup_error.go:259) with two
// documented differences: insufficient_quota and quota_exceeded are excluded
// because retrying them on the same site is pointless (429-vs-quota.md, third
// row), and a message containing "Rate limit reached" is accepted on its own
// (interface 2.1).
func IsRateLimit(e UpstreamError) bool {
	code := strings.ToLower(strings.TrimSpace(codeText(e.Code)))
	text := strings.NewReplacer("-", "_", " ", "_", ":", "_").Replace(code + " " + strings.ToLower(e.Type))

	return code == "429" ||
		containsAny(text, "rate_limit_exceeded", "rate_limit_error", "ratelimitreached", "too_many_requests", "toomanyrequests") ||
		strings.Contains(strings.ToLower(e.Message), "rate limit reached")
}

func containsAny(text string, values ...string) bool {
	for _, value := range values {
		if strings.Contains(text, value) {
			return true
		}
	}
	return false
}

// codeText renders a code for matching: a JSON string loses its quotes and
// escapes, any other JSON value keeps its textual form ("429", "429.5").
func codeText(raw json.RawMessage) string {
	if !hasJSON(raw) {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(bytes.TrimSpace(raw))
}

// stringValue is the value of a JSON string, or "" for anything else (null, a
// number, an object). It mirrors GetOpenAIError, which only fills Param from a
// string field.
func stringValue(raw json.RawMessage) string {
	if !hasJSON(raw) {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// hasJSON reports whether raw holds a JSON value other than null or nothing.
func hasJSON(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

// cloneRaw returns a copy of raw that does not alias the decoder buffer, and
// nil when there is no value to keep.
func cloneRaw(raw json.RawMessage) json.RawMessage {
	if !hasJSON(raw) {
		return nil
	}
	return append(json.RawMessage(nil), bytes.TrimSpace(raw)...)
}
