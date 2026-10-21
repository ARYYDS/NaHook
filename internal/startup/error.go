package startup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// failureMessage is what H3 puts into an otherwise empty error of a failed
// startup event (responses_startup_error.go:232).
const failureMessage = "Responses startup event reported failure"

// upstreamError extracts the error fields from one frame exactly the way H3
// responsesStartupFrameError does (responses_startup_error.go:205-245,
// interface 2.1):
//
//  1. frame.error → frame.response.error → the frame's own code/message when the
//     event type is "error" (that last branch carries no Type and no Param,
//     like H3, so the 429 body stays byte-identical to H3's);
//  2. when the event is response.failed or response.error, or response.status
//     is "failed", a frame without an error falls back to its own code/message,
//     and an empty message becomes failureMessage (H3 :220-232).
//
// A JSON null counts as absent. The fields are returned as they are: no status
// mapping, no message rewrite beyond the H3 fallback.
func upstreamError(eventType string, f *frame) (UpstreamError, bool) {
	var e UpstreamError
	ok := true
	switch {
	case hasJSON(f.Error):
		e = errorValue(f.Error)
	case f.Response != nil && hasJSON(f.Response.Error):
		e = errorValue(f.Response.Error)
	case strings.EqualFold(eventType, "error"):
		e = UpstreamError{Message: f.Message, Code: canonical(f.Code)}
	default:
		ok = false
	}

	failed := strings.EqualFold(eventType, "response.failed") ||
		strings.EqualFold(eventType, "response.error") ||
		(f.Response != nil && strings.EqualFold(stringValue(f.Response.Status), "failed"))
	if failed {
		if !ok {
			e, ok = UpstreamError{Message: f.Message, Code: canonical(f.Code)}, true
		}
		if strings.TrimSpace(e.Message) == "" {
			e.Message = failureMessage
		}
	}
	return e, ok
}

// errorValue maps one JSON error value the way dto.GetOpenAIError does
// (NEWAPI/relaykit/dto/openai_response.go:605): an object contributes type,
// message and param only when they are strings, and code whatever it is; a bare
// string becomes Type "error"; any other JSON value becomes Type
// "unknown_error". The caller has already checked that raw is not null.
func errorValue(raw json.RawMessage) UpstreamError {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return UpstreamError{Message: string(bytes.TrimSpace(raw)), Type: "unknown_error"}
	}
	switch val := v.(type) {
	case map[string]any:
		var e UpstreamError
		e.Type, _ = val["type"].(string)
		e.Message, _ = val["message"].(string)
		e.Param, _ = val["param"].(string)
		if code, present := val["code"]; present && code != nil {
			e.Code, _ = json.Marshal(code)
		}
		return e
	case string:
		return UpstreamError{Message: val, Type: "error"}
	default:
		return UpstreamError{Message: fmt.Sprintf("%v", val), Type: "unknown_error"}
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

// codeText renders a code the way H3 does before matching:
// fmt.Sprint(upstreamErr.Code) on the decoded JSON value, so a string keeps its
// text and 429.0 reads as "429".
func codeText(raw json.RawMessage) string {
	if !hasJSON(raw) {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// canonical decodes a JSON value and encodes it again with encoding/json. H3
// keeps OpenAIError.Code as any and gin encodes it on the way out, so this is
// the byte form H3 would answer with (429.0 → 429, "<" and "<" both →
// "<"). It returns nil for null or invalid input.
func canonical(raw json.RawMessage) json.RawMessage {
	if !hasJSON(raw) {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// stringValue is the value of a JSON string, or "" for anything else (null, a
// number, an object).
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
