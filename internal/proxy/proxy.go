// Package proxy is nahook's data plane (design §1.2, §1.6, §1.8, §6.1;
// interface 2.5). It classifies each request, forwards it to NewAPI and, only
// for a streaming request whose startup frames carry an upstream rate limit,
// retries the same request behind the cooldown gate and answers a stand-in 429
// once the retry ceiling is reached.
//
// Nothing here writes to the ResponseWriter before the decision is final: the
// 429 is only possible while the client has not received a byte (design §1.6).
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/textproto"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/ARYYDS/NaHook/internal/config"
	"github.com/ARYYDS/NaHook/internal/gate"
	"github.com/ARYYDS/NaHook/internal/obs"
)

// reqMeta is what classify learned about an intercepted request.
type reqMeta struct {
	Model string
}

// Handler serves the data plane. One Handler is shared by every request; it
// holds no per-request state.
type Handler struct {
	store  *config.Store
	gates  *gate.Registry
	st     *obs.Stats
	log    *slog.Logger
	base   *url.URL
	client *http.Client
}

// NewHandler builds the data plane. The transport carries exactly the four
// settings of design §6.1: compression is off because the startup frames must
// be read as plain text, and there is no ResponseHeaderTimeout — Caddy's
// response_header_timeout is the outer bound (design §4).
//
// A published config never changes newapi (Store.Reload refuses that with
// ErrRestartRequired), so the base URL is fixed for the process lifetime.
func NewHandler(s *config.Store, g *gate.Registry, st *obs.Stats, log *slog.Logger) *Handler {
	base, err := url.Parse(s.Current().NewAPI)
	if err != nil || base == nil {
		// config.Validate rejects such a base before NewHandler is reached; an
		// empty base keeps forward failing cleanly (502) instead of panicking.
		base = &url.URL{}
	}
	return &Handler{
		store: s,
		gates: g,
		st:    st,
		log:   log,
		base:  base,
		client: &http.Client{Transport: &http.Transport{
			DisableCompression:  true,
			MaxIdleConns:        2000,
			MaxIdleConnsPerHost: 2000,
			IdleConnTimeout:     10 * time.Minute,
		}},
	}
}

// ServeHTTP classifies the request and either forwards it once (pass-through)
// or runs the intercept loop (design §1.6).
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cfg := h.store.Current()
	body, meta, intercept, rest, err := h.classify(r, cfg)
	if err != nil {
		h.fail(w, r)
		return
	}
	if intercept {
		h.serveIntercept(w, r, body, meta)
		return
	}

	src := rest
	if src == nil {
		// classify read the body into body and did not leave a reader behind.
		src = bytes.NewReader(body)
	}
	resp, err := h.forward(r.Context(), r, src)
	if err != nil {
		h.fail(w, r)
		return
	}
	h.st.Inc(obs.Passthrough)
	passthrough(w, resp)
}

// fail reports a forward or body-read failure (design §1.6). A client that is
// already gone is not a NewAPI fault: it only counts client_gone and writes
// nothing, because there may be nobody left to read the answer. Everything
// else counts bad_gateway and answers 502.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request) {
	if r.Context().Err() != nil {
		h.st.Inc(obs.ClientGone)
		return
	}
	h.st.Inc(obs.BadGateway)
	writeBadGateway(w)
}

// classify decides whether one request may be intercepted (design §1.2).
//
// It returns the buffered body for the intercept path; the reader to forward
// when the body was too large to buffer (buf followed by the unread request
// body); or a nil reader when the body is returned whole in body. Only a body
// read error is an error; a bad or non-streaming body is passed through.
func (h *Handler) classify(r *http.Request, cfg *config.Config) (body []byte, meta reqMeta, intercept bool, rest io.Reader, err error) {
	if r.Method != http.MethodPost || !slices.Contains(cfg.Match.Paths, r.URL.Path) {
		return nil, reqMeta{}, false, r.Body, nil
	}

	buf, readErr := io.ReadAll(io.LimitReader(r.Body, cfg.BodyLimit+1))
	if readErr != nil {
		return nil, reqMeta{}, false, nil, readErr
	}
	if int64(len(buf)) > cfg.BodyLimit {
		// Too large to inspect: forward what was read and the rest of the body
		// unchanged, without intercepting.
		return nil, reqMeta{}, false, io.MultiReader(bytes.NewReader(buf), r.Body), nil
	}

	var head struct {
		Stream *bool  `json:"stream"`
		Model  string `json:"model"`
	}
	if json.Unmarshal(buf, &head) != nil || head.Stream == nil || !*head.Stream {
		return buf, reqMeta{}, false, nil, nil
	}
	if len(cfg.Match.Models) > 0 && !slices.Contains(cfg.Match.Models, head.Model) {
		return buf, reqMeta{}, false, nil, nil
	}
	return buf, reqMeta{Model: head.Model}, true, nil, nil
}

// hopByHop lists the headers that are specific to one connection (RFC 9110
// §7.6.1). Fields named by Connection are added per request by hopHeaders.
var hopByHop = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Connection":    true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// hopHeaders returns the hop-by-hop set for one header map, including the field
// names listed in its Connection header.
func hopHeaders(h http.Header) map[string]bool {
	drop := make(map[string]bool, len(hopByHop)+2)
	for name := range hopByHop {
		drop[name] = true
	}
	for _, value := range h.Values("Connection") {
		for name := range strings.SplitSeq(value, ",") {
			if name = strings.TrimSpace(name); name != "" {
				drop[textproto.CanonicalMIMEHeaderKey(name)] = true
			}
		}
	}
	return drop
}

// forward sends one attempt to NewAPI (design §6.1).
//
// The URL is the configured newapi base plus the request path (RawPath
// included) and query; RequestURI stays empty because a client request that
// sets it cannot be written. Host is the client's Host (Caddy forwards it
// unchanged, so NewAPI keeps seeing the host the client used). Headers are
// copied minus the hop-by-hop set, Accept-Encoding becomes identity so the
// startup frames are readable, and X-Forwarded-For, X-Real-IP and Authorization
// are left exactly as the client sent them.
//
// For a *bytes.Reader, http.NewRequestWithContext fills in ContentLength and
// GetBody by itself, so transports can replay the body. For the streaming
// pass-through the length comes from the client request instead.
func (h *Handler) forward(ctx context.Context, r *http.Request, body io.Reader) (*http.Response, error) {
	u := *h.base
	u.Path = joinPath(h.base.Path, r.URL.Path)
	u.RawPath = r.URL.RawPath
	if h.base.Path != "" && h.base.Path != "/" {
		// The base path changed the encoding context, so let net/url encode the
		// joined path rather than reusing the client's RawPath.
		u.RawPath = ""
	}
	u.RawQuery = r.URL.RawQuery
	u.Fragment = ""

	req, err := http.NewRequestWithContext(ctx, r.Method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if _, ok := body.(*bytes.Reader); !ok {
		req.ContentLength = r.ContentLength
	}
	req.Host = r.Host

	drop := hopHeaders(r.Header)
	for name, values := range r.Header {
		canonical := textproto.CanonicalMIMEHeaderKey(name)
		// Host and Content-Length travel as Request fields, not headers.
		if drop[canonical] || canonical == "Host" || canonical == "Content-Length" {
			continue
		}
		for _, value := range values {
			req.Header.Add(canonical, value)
		}
	}
	req.Header.Set("Accept-Encoding", "identity")

	return h.client.Do(req)
}

// joinPath appends the client path to a base path. An empty base path keeps the
// client path as it is, which is the deployed shape (newapi is a bare origin).
func joinPath(base, path string) string {
	if base == "" || base == "/" {
		return path
	}
	return strings.TrimSuffix(base, "/") + path
}
