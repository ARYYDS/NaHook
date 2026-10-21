// Package proxy429 writes the stand-in 429 that nahook returns once its retries
// are used up (design §1.4, interface 2.2).
//
// The body is byte-for-byte what NewAPI (h3) returns for an upstream OpenAI
// error: the field order of NEWAPI relaykit/types/error.go:13-19, json.Marshal
// rather than an Encoder so there is no trailing newline, and the upstream code
// value passed through unchanged.
package proxy429

import (
	"encoding/json"
	"net/http"

	"github.com/ARYYDS/NaHook/internal/startup"
)

// contentType is what NEWAPI controller/relay.go:105 sets before c.JSON.
const contentType = "application/json; charset=utf-8"

// envelope and openAIError mirror NEWAPI relaykit/types/error.go:13-19. The
// field order is the wire order. metadata is not emitted: it is omitempty there
// and nahook never produces one.
type envelope struct {
	Error openAIError `json:"error"`
}

type openAIError struct {
	Message string          `json:"message"`
	Type    string          `json:"type"`
	Param   string          `json:"param"`
	Code    json.RawMessage `json:"code"`
}

// Write answers the client with a 429 in NewAPI's format. It must be the first
// write to w: the caller decides to answer 429 only after it knows that nothing
// has been sent yet (design §1.6).
//
// requestID is the X-Oneapi-Request-Id of the last upstream attempt; it is
// copied as is and the header is omitted when it is empty. The message is never
// rewritten and no Retry-After is added, so the response stays consistent with
// what NewAPI itself would have produced.
func Write(w http.ResponseWriter, requestID string, e startup.UpstreamError) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	if requestID != "" {
		h.Set("X-Oneapi-Request-Id", requestID)
	}
	w.WriteHeader(http.StatusTooManyRequests)

	body, err := json.Marshal(envelope{Error: openAIError{
		Message: e.Message,
		Type:    e.Type,
		Param:   e.Param,
		Code:    e.Code, // already canonical from startup; nil marshals as null
	}})
	if err != nil {
		// Unreachable while Code comes from startup's canonical encoder: the
		// three string fields always marshal. The 429 is already committed, so
		// there is nothing left to change.
		return
	}
	_, _ = w.Write(body)
}
