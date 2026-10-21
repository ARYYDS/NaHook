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
	"net/textproto"
	"strings"

	"github.com/ARYYDS/NaHook/internal/startup"
)

// contentType is what NEWAPI controller/relay.go:105 sets before c.JSON.
const contentType = "application/json; charset=utf-8"

// copied lists the headers taken from the last upstream attempt, besides every
// Access-Control-* header: NewAPI's middleware adds them to every response, so
// h3's own 429 carries them too (design §1.4).
var copied = map[string]bool{
	"X-Oneapi-Request-Id": true, // NEWAPI middleware/request-id.go:16
	"X-New-Api-Version":   true, // NEWAPI middleware/cors.go:20, mounted globally at main.go:198
	"Vary":                true, // CORS, NEWAPI router/relay-router.go:16
}

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
// upstream is the response header of the last upstream attempt. Only the
// request id, X-New-Api-Version, Vary and the Access-Control-* headers are
// copied; SSE, hop-by-hop, Content-* and forwarded headers are not. The message
// is never rewritten and no Retry-After is added, so the response stays
// consistent with what NewAPI itself would have produced.
func Write(w http.ResponseWriter, upstream http.Header, e startup.UpstreamError) {
	oe := openAIError{
		Message: e.Message,
		Type:    e.Type,
		Param:   e.Param,
		Code:    e.Code, // already canonical from startup; nil marshals as null
	}
	// Marshal before committing the status, so the 429 always has a JSON body.
	body, err := json.Marshal(envelope{Error: oe})
	if err != nil {
		// Unreachable while Code comes from startup's canonical encoder; the
		// three string fields always marshal.
		oe.Code = nil
		body, _ = json.Marshal(envelope{Error: oe})
	}

	h := w.Header()
	for k, v := range upstream {
		ck := textproto.CanonicalMIMEHeaderKey(k)
		if copied[ck] || strings.HasPrefix(ck, "Access-Control-") {
			h[ck] = append([]string(nil), v...)
		}
	}
	h.Set("Content-Type", contentType)
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write(body)
}
