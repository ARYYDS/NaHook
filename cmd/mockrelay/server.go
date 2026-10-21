package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The only two business paths mockrelay serves (design §6.6).
const (
	pathResponses = "/v1/responses"
	pathChat      = "/v1/chat/completions"
)

type itemKind int

const (
	kindOK itemKind = iota
	kindSlow
	kindHold
	kindRateLimit
	kindRateLimitFailedOnly
	kindHoldRateLimit
	kindStallRateLimit
	kindChatRateLimit
	kindQuota
	kindHTTP429
	kindHoldHTTP429
)

// pathKind says where a script item applies.
type pathKind int

const (
	pathBoth pathKind = iota
	pathResponsesOnly
	pathChatOnly
)

// scriptItem is one parsed entry of /__mode's script.
type scriptItem struct {
	kind    itemKind
	raw     string // exactly as the operator wrote it, for /__log and errors
	seconds int
	paths   pathKind
}

// parseItem validates one script entry. Anything unknown, any missing or
// non-positive second count, and any parameter on an item that takes none is an
// error, so /__mode can reject the whole script without touching the old one.
func parseItem(raw string) (scriptItem, error) {
	name, arg, hasArg := strings.Cut(raw, ":")
	item := scriptItem{raw: raw}

	switch name {
	case "ok":
		item.kind = kindOK
	case "slow":
		item.kind = kindSlow
	case "hold":
		item.kind = kindHold
	case "rate_limit":
		item.kind = kindRateLimit
	case "rate_limit_failed_only":
		item.kind = kindRateLimitFailedOnly
	case "hold_rate_limit":
		item.kind = kindHoldRateLimit
	case "stall_rate_limit":
		item.kind = kindStallRateLimit
	case "chat_rate_limit":
		item.kind = kindChatRateLimit
	case "quota":
		item.kind = kindQuota
	case "http429":
		item.kind = kindHTTP429
	case "hold_http429":
		item.kind = kindHoldHTTP429
	default:
		return scriptItem{}, fmt.Errorf("unknown item %q", raw)
	}

	if takesSeconds(item.kind) {
		if !hasArg {
			return scriptItem{}, fmt.Errorf("item %q needs a number of seconds", raw)
		}
		seconds, err := strconv.Atoi(arg)
		if err != nil || seconds < 1 {
			return scriptItem{}, fmt.Errorf("item %q needs a positive whole number of seconds", raw)
		}
		item.seconds = seconds
	} else if hasArg {
		return scriptItem{}, fmt.Errorf("item %q does not take a parameter", raw)
	}

	switch item.kind {
	case kindRateLimit, kindRateLimitFailedOnly, kindHoldRateLimit, kindStallRateLimit, kindQuota:
		item.paths = pathResponsesOnly
	case kindChatRateLimit:
		item.paths = pathChatOnly
	default:
		item.paths = pathBoth
	}
	return item, nil
}

func takesSeconds(kind itemKind) bool {
	switch kind {
	case kindSlow, kindHold, kindHoldRateLimit, kindStallRateLimit, kindHoldHTTP429:
		return true
	default:
		return false
	}
}

// appliesTo reports whether the item may answer this path.
func (i scriptItem) appliesTo(path string) bool {
	switch i.paths {
	case pathResponsesOnly:
		return path == pathResponses
	case pathChatOnly:
		return path == pathChat
	default:
		return path == pathResponses || path == pathChat
	}
}

// logEntry is one row of GET /__log (design §6.6).
type logEntry struct {
	Seq      int    `json:"seq"`
	Path     string `json:"path"`
	Item     string `json:"item"`
	AtUnixMS int64  `json:"at_unix_ms"`
}

// server is the whole state of mockrelay: the script, the request counter and
// the arrival log, all behind one mutex.
type server struct {
	mu     sync.Mutex
	script []scriptItem
	count  int
	log    []logEntry
}

func newHandler() http.Handler {
	s := &server{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+pathResponses, s.serveBusiness)
	mux.HandleFunc("POST "+pathChat, s.serveBusiness)
	mux.HandleFunc("POST /__mode", s.handleMode)
	mux.HandleFunc("GET /__log", s.handleLog)
	return mux
}

// take consumes the next script item, bumps the counter and records the arrival,
// all under the same lock, so concurrent requests never share or skip an item
// (T-F4). It returns ok=false when no script has been set yet.
func (s *server) take(path string, atUnixMS int64) (scriptItem, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.count++
	seq := s.count

	var item scriptItem
	ok := len(s.script) > 0
	if ok {
		index := seq - 1
		if index >= len(s.script) {
			index = len(s.script) - 1 // the last item repeats forever
		}
		item = s.script[index]
	}
	s.log = append(s.log, logEntry{Seq: seq, Path: path, Item: item.raw, AtUnixMS: atUnixMS})
	return item, ok
}

func (s *server) serveBusiness(w http.ResponseWriter, r *http.Request) {
	at := time.Now().UnixMilli()
	item, ok := s.take(r.URL.Path, at)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "mockrelay: no script configured")
		return
	}
	if !item.appliesTo(r.URL.Path) {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("mockrelay: item %s does not apply to %s", item.raw, r.URL.Path))
		return
	}

	ctx := r.Context()
	sw := newStreamWriter(w)
	delay := time.Duration(item.seconds) * time.Second

	switch item.kind {
	case kindOK:
		_ = writeOK(sw, ctx, r.URL.Path, len(deltaTexts), 0)
	case kindSlow:
		_ = writeOK(sw, ctx, r.URL.Path, item.seconds, time.Second)
	case kindHold:
		if sleepCtx(ctx, delay) == nil {
			_ = writeOK(sw, ctx, r.URL.Path, len(deltaTexts), 0)
		}
	case kindRateLimit:
		_ = writeResponsesRateLimit(sw, rateLimitError)
	case kindRateLimitFailedOnly:
		_ = sw.start()
		_ = sw.write(responsesFailedFrame(rateLimitError))
	case kindHoldRateLimit:
		if sleepCtx(ctx, delay) == nil {
			_ = writeResponsesRateLimit(sw, rateLimitError)
		}
	case kindStallRateLimit:
		// Commit 200 and the headers, then stay silent for N seconds so NewAPI
		// starts its in-stream keep-alive (T-F15).
		if err := sw.start(); err == nil && sleepCtx(ctx, delay) == nil {
			_ = writeResponsesRateLimit(sw, rateLimitError)
		}
	case kindChatRateLimit:
		_ = writeChatRateLimit(sw)
	case kindQuota:
		_ = writeResponsesRateLimit(sw, quotaError)
	case kindHTTP429:
		writeHTTP429(w)
	case kindHoldHTTP429:
		if sleepCtx(ctx, delay) == nil {
			writeHTTP429(w)
		}
	}
}

// writeOK streams the ok sequence for the path: three deltas, or count deltas
// spaced by interval for slow.
func writeOK(sw *streamWriter, ctx context.Context, path string, count int, interval time.Duration) error {
	if path == pathChat {
		return writeChatOK(sw, ctx, count, interval)
	}
	return writeResponsesOK(sw, ctx, count, interval)
}

func writeResponsesOK(sw *streamWriter, ctx context.Context, count int, interval time.Duration) error {
	if err := sw.start(); err != nil {
		return err
	}
	if err := sw.write(responsesCreatedFrame()); err != nil {
		return err
	}
	for i := 0; i < count; i++ {
		if err := sleepCtx(ctx, interval); err != nil {
			return err
		}
		if err := sw.write(responsesDeltaFrame(deltaTexts[i%len(deltaTexts)])); err != nil {
			return err
		}
	}
	return sw.write(responsesCompletedFrame())
}

func writeChatOK(sw *streamWriter, ctx context.Context, count int, interval time.Duration) error {
	if err := sw.start(); err != nil {
		return err
	}
	for i := 0; i < count; i++ {
		if err := sleepCtx(ctx, interval); err != nil {
			return err
		}
		if err := sw.write(chatChunkFrame(deltaTexts[i%len(deltaTexts)])); err != nil {
			return err
		}
	}
	if err := sw.write(chatStopFrame()); err != nil {
		return err
	}
	return sw.write(chatDoneFrame)
}

// writeResponsesRateLimit writes created → error → response.failed.
func writeResponsesRateLimit(sw *streamWriter, errJSON string) error {
	if err := sw.start(); err != nil {
		return err
	}
	if err := sw.write(responsesCreatedFrame()); err != nil {
		return err
	}
	if err := sw.write(responsesErrorFrame(errJSON)); err != nil {
		return err
	}
	return sw.write(responsesFailedFrame(errJSON))
}

func writeChatRateLimit(sw *streamWriter) error {
	if err := sw.start(); err != nil {
		return err
	}
	if err := sw.write(chatErrorFrame(rateLimitError)); err != nil {
		return err
	}
	return sw.write(chatDoneFrame)
}

func writeHTTP429(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = io.WriteString(w, http429Body)
}

func (s *server) handleMode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Script []string `json:"script"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "mockrelay: invalid JSON body")
		return
	}
	if len(req.Script) == 0 {
		writeError(w, http.StatusBadRequest, "mockrelay: script is empty")
		return
	}

	items := make([]scriptItem, 0, len(req.Script))
	for _, raw := range req.Script {
		item, err := parseItem(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "mockrelay: "+err.Error())
			return
		}
		items = append(items, item)
	}

	// Only a fully valid script replaces the old one, and the counter and the
	// arrival log are cleared at the same moment.
	s.mu.Lock()
	s.script = items
	s.count = 0
	s.log = nil
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, struct {
		Script []string `json:"script"`
	}{Script: req.Script})
}

func (s *server) handleLog(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	entries := make([]logEntry, len(s.log))
	copy(entries, s.log)
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, struct {
		Count    int        `json:"count"`
		Requests []logEntry `json:"requests"`
	}{Count: len(entries), Requests: entries})
}

type errorEnvelope struct {
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, status int, message string) {
	var body errorEnvelope
	body.Error.Message = message
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
