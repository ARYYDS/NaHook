package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/textproto"
)

// release writes the upstream response that the decision approved: the status
// and headers from NewAPI, the startup prefix that was already read, and then
// the rest of the stream chunk by chunk (design §1.8).
//
// The bytes are exactly the ones NewAPI sent, so billing at NewAPI and the
// client's view stay identical.
func release(w http.ResponseWriter, resp *http.Response, prefix []byte, rest io.Reader) {
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(prefix)
	flush(w)
	copyFlush(w, rest)
	_ = resp.Body.Close()
}

// passthrough releases a response that was never inspected (design §1.8).
func passthrough(w http.ResponseWriter, resp *http.Response) {
	release(w, resp, nil, resp.Body)
}

// copyFlush streams rest to the client, flushing after every chunk so the
// client sees each SSE frame as soon as it arrives (design §1.8).
func copyFlush(w http.ResponseWriter, src io.Reader) {
	if src == nil {
		return
	}
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
			flush(w)
		}
		if err != nil {
			return
		}
	}
}

// flush pushes buffered bytes to the client. A ResponseWriter that cannot flush
// (a test recorder, a closed connection) is not an error.
func flush(w http.ResponseWriter) {
	_ = http.NewResponseController(w).Flush()
}

// copyHeaders copies the response headers minus the hop-by-hop set.
func copyHeaders(dst, src http.Header) {
	drop := hopHeaders(src)
	for name, values := range src {
		canonical := textproto.CanonicalMIMEHeaderKey(name)
		if drop[canonical] {
			continue
		}
		dst[canonical] = append([]string(nil), values...)
	}
}

// errorEnvelope and errorFields are the 502 answer of design §6.1. The field
// order is the wire order, matching the 429 envelope.
type errorEnvelope struct {
	Error errorFields `json:"error"`
}

type errorFields struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   string `json:"param"`
	Code    string `json:"code"`
}

// writeBadGateway answers 502 when NewAPI cannot be reached (design §6.1).
func writeBadGateway(w http.ResponseWriter) {
	body := errorEnvelope{Error: errorFields{
		Message: "nahook: upstream unavailable",
		Type:    "nahook_error",
		Param:   "",
		Code:    "upstream_unavailable",
	}}

	data, err := json.Marshal(body)
	if err != nil {
		http.Error(w, "nahook: upstream unavailable", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusBadGateway)
	_, _ = w.Write(data)
}
