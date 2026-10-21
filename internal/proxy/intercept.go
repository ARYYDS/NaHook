package proxy

import (
	"bytes"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/ARYYDS/NaHook/internal/gate"
	"github.com/ARYYDS/NaHook/internal/obs"
	"github.com/ARYYDS/NaHook/internal/proxy429"
	"github.com/ARYYDS/NaHook/internal/startup"
)

// serveIntercept runs the intercept loop of design §1.6: forward the buffered
// request, inspect the startup frames, wait behind the gate and forward again
// until the retry ceiling is reached, then answer the stand-in 429.
//
// Every decision point re-reads the configuration, so a retry ceiling changed
// through the control socket applies at the next interception.
func (h *Handler) serveIntercept(w http.ResponseWriter, r *http.Request, body []byte, meta reqMeta) {
	n := 0 // interceptions of this request so far
	h.st.Inc(obs.Intercepted)

	for {
		t0 := time.Now()
		resp, err := h.forward(r.Context(), r, bytes.NewReader(body))
		if err != nil {
			if r.Context().Err() != nil {
				h.st.Inc(obs.ClientGone)
				return
			}
			h.st.Inc(obs.BadGateway)
			writeBadGateway(w)
			return
		}

		// rid identifies this NewAPI request in logs and in its own final error.
		rid := resp.Header.Get("X-Oneapi-Request-Id")
		if resp.StatusCode != http.StatusOK || !isEventStream(resp) {
			// Includes NewAPI's own HTTP 429: it already retried per RetryTimes.
			h.st.Inc(obs.Passthrough)
			passthrough(w, resp)
			return
		}

		res := startup.Read(resp.Body)
		if res.Verdict == startup.VerdictRateLimit && newAPIFinalError(res, rid) {
			// NewAPI's own terminal error: it committed 200 with its heartbeat,
			// exhausted RetryTimes and wrote the error into the stream (the two
			// ways to recognise it are on newAPIFinalError). Retrying here would
			// stack two retry loops.
			h.st.Inc(obs.Passthrough)
			release(w, resp, res.Prefix, res.Rest)
			return
		}
		if res.Verdict == startup.VerdictRelease {
			if r.Context().Err() != nil {
				// The read ended because the client went away: there is nobody to
				// release to, and a release that never happened must not reset
				// the gates of this model (design §1.6).
				_ = resp.Body.Close()
				h.st.Inc(obs.ClientGone)
				return
			}
			h.gates.ResetModel(meta.Model)
			h.st.Inc(obs.Released)
			h.log.Info("release", "request_id", rid, "attempt", n)
			release(w, resp, res.Prefix, res.Rest)
			return
		}

		// Rate limited: this attempt is abandoned, so drop its body at once.
		_ = resp.Body.Close()
		h.st.Inc(obs.RateLimited)

		cfg := h.store.Current()
		key := gate.Key(meta.Model, res.Err.Message)
		g := h.gates.Get(key)
		elapsed := time.Since(t0)
		g.OnRateLimit(elapsed, time.Duration(cfg.Gate.BaseFloorMS)*time.Millisecond, cfg.Gate.StreakCap)
		max := h.store.RetryMax(meta.Model)
		h.log.Info("intercept", "request_id", rid, "key", key, "attempt", n, "elapsed_ms", elapsed.Milliseconds(), "max", max)

		if n >= max {
			h.st.Inc(obs.GaveUp)
			h.log.Info("give_up", "request_id", rid, "key", key, "attempts", n+1)
			proxy429.Write(w, resp.Header, res.Err)
			return
		}

		n++
		h.st.Inc(obs.Retries)
		w0 := time.Now()
		if g.Wait(r.Context()) != nil {
			h.st.Inc(obs.ClientGone)
			return
		}
		h.log.Info("wait", "key", key, "wait_ms", time.Since(w0).Milliseconds())
	}
}

// isEventStream reports whether NewAPI answered with an SSE body. Anything else
// is passed through untouched (design §1.6).
func isEventStream(resp *http.Response) bool {
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	return err == nil && mediaType == "text/event-stream"
}

// newAPIFinalError reports whether a rate-limit verdict is NewAPI's own terminal
// error rather than an upstream frame; retrying it here would stack a second
// retry loop on top of NewAPI's RetryTimes (design §1.6). Either rule is enough:
//
//  1. the message ends with " (request id: <rid>)", the suffix Relay's defer adds
//     with MessageWithRequestId for the errors NewAPI generates itself. rid must
//     be the X-Oneapi-Request-Id of this very response: a frame forwarded
//     verbatim cannot carry it, and a relay that runs its own NewAPI carries a
//     different one.
//  2. the deciding frame was preceded by heartbeats only and carried no type.
//     NewAPI committed 200 with its heartbeat, exhausted RetryTimes and wrote the
//     forwarded upstream error with writeRelayStreamError. That error keeps the
//     upstream object unchanged, so rule 1 cannot see it.
//
// Rule 2 can only misjudge towards passing the stream through (a slow upstream
// whose first frame is an untyped rate limit, e.g. the Chat shape); the client
// then sees exactly what the official behaviour would deliver (design §8).
func newAPIFinalError(res startup.Result, requestID string) bool {
	if requestID != "" && strings.HasSuffix(strings.TrimSpace(res.Err.Message), " (request id: "+requestID+")") {
		return true
	}
	return res.HeartbeatOnly && res.Untyped
}
