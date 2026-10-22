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
	"strconv"
	"strings"
	"time"

	"github.com/ARYYDS/NaHook/internal/startup"
)

const (
	// contentType is what NEWAPI controller/relay.go:105 sets before c.JSON.
	contentType = "application/json; charset=utf-8"

	// maxRetryAfter bounds Retry-After to the same 30m as gate.maxBase
	// (design §1.4): the base of a cooldown window never exceeds it, so a
	// remaining time above the bound can only come from a streak widening the
	// window, and 1800 seconds is still a sane thing to ask a client to wait.
	maxRetryAfter = 30 * time.Minute
)

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
// is never rewritten, so besides Retry-After the response stays byte-for-byte
// what NewAPI itself would have produced.
//
// retryAfter is what is left of the cooldown window of this key
// (gate.(*Gate).Remaining). When it is positive the answer carries
// Retry-After, the only header nahook adds on its own (design §1.4): NewAPI's
// relay errors never have one, and a client that backs off for less than the
// window would run into the same limit again.
func Write(w http.ResponseWriter, upstream http.Header, e startup.UpstreamError, retryAfter time.Duration) {
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
	if retryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(retryAfterSeconds(retryAfter)))
	}
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write(body)
}

// retryAfterSeconds renders the remaining window as the delay-seconds value of
// RFC 9110 §10.2.3: a decimal integer, rounded up so that a client which waits
// exactly that long is past the window, never 0, and capped at 1800 (design
// §1.4). Rounding down would let the client retry inside the window; writing 0
// would tell it to retry at once.
func retryAfterSeconds(remain time.Duration) int {
	secs := (min(remain, maxRetryAfter) + time.Second - 1) / time.Second
	if secs < 1 {
		secs = 1
	}
	return int(secs)
}
