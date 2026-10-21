package main

import (
	"context"
	"io"
	"net/http"
	"time"
)

// Fixed frame contents (design §6.6). Nothing here carries a timestamp or a
// random value, because T-F5 and T-F6 compare the bytes of two separate
// requests. id, created and model are pinned: resp_mock / chatcmpl-mock,
// 1760000000, gpt-6-astra.
const (
	// rateLimitMessage is <RL> verbatim (渠道监控/429-vs-quota.md:12-14).
	rateLimitMessage = "Rate limit reached for gpt-6-astra in organization org-mock on tokens per min (TPM): " +
		"Limit 40000000, Used 40000000, Requested 19224. Please try again in 28ms."

	quotaMessage = "You exceeded your current quota, please check your plan and billing details."

	// rateLimitError is <RLE>.
	rateLimitError = `{"type":"tokens","code":"rate_limit_exceeded","message":"` + rateLimitMessage + `"}`
	quotaError     = `{"type":"insufficient_quota","code":"insufficient_quota","message":"` + quotaMessage + `"}`

	// http429Body is what http429 and hold_http429 answer.
	http429Body = `{"error":{"message":"` + rateLimitMessage + `","type":"tokens","param":"","code":"rate_limit_exceeded"}}`
)

// deltaTexts is the fixed content of the three assistant deltas.
var deltaTexts = []string{"Hello", " world", "!"}

// sseFrame renders one Responses frame: an event line plus a data line.
func sseFrame(event, data string) string {
	return "event: " + event + "\ndata: " + data + "\n\n"
}

// chatFrame renders one Chat Completions frame: a data line only.
func chatFrame(data string) string {
	return "data: " + data + "\n\n"
}

const chatDoneFrame = "data: [DONE]\n\n"

func responsesCreatedFrame() string {
	return sseFrame("response.created",
		`{"type":"response.created","response":{"id":"resp_mock","object":"response","created_at":1760000000,"model":"gpt-6-astra","status":"in_progress"}}`)
}

func responsesDeltaFrame(text string) string {
	return sseFrame("response.output_text.delta",
		`{"type":"response.output_text.delta","item_id":"msg_mock","output_index":0,"content_index":0,"delta":"`+text+`"}`)
}

func responsesCompletedFrame() string {
	return sseFrame("response.completed",
		`{"type":"response.completed","response":{"id":"resp_mock","object":"response","created_at":1760000000,"model":"gpt-6-astra","status":"completed","usage":{"input_tokens":10,"output_tokens":3,"total_tokens":13}}}`)
}

func responsesErrorFrame(errJSON string) string {
	return sseFrame("error", `{"type":"error","error":`+errJSON+`}`)
}

func responsesFailedFrame(errJSON string) string {
	return sseFrame("response.failed",
		`{"type":"response.failed","response":{"id":"resp_mock","status":"failed","error":`+errJSON+`}}`)
}

func chatChunkFrame(text string) string {
	return chatFrame(`{"id":"chatcmpl-mock","object":"chat.completion.chunk","created":1760000000,"model":"gpt-6-astra","choices":[{"index":0,"delta":{"content":"` + text + `"},"finish_reason":null}]}`)
}

func chatStopFrame() string {
	return chatFrame(`{"id":"chatcmpl-mock","object":"chat.completion.chunk","created":1760000000,"model":"gpt-6-astra","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13}}`)
}

func chatErrorFrame(errJSON string) string {
	return chatFrame(`{"error":` + errJSON + `}`)
}

// streamWriter writes one SSE response and flushes after every frame.
type streamWriter struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	started bool
}

func newStreamWriter(w http.ResponseWriter) *streamWriter {
	return &streamWriter{w: w, rc: http.NewResponseController(w)}
}

// start commits 200 and the SSE headers. It is idempotent so that a caller can
// commit early (stall_rate_limit) and still reuse the frame writers.
func (s *streamWriter) start() error {
	if s.started {
		return nil
	}
	s.started = true
	s.w.Header().Set("Content-Type", "text/event-stream")
	s.w.WriteHeader(http.StatusOK)
	return s.rc.Flush()
}

func (s *streamWriter) write(frame string) error {
	if _, err := io.WriteString(s.w, frame); err != nil {
		return err
	}
	return s.rc.Flush()
}

// sleepCtx waits for d, or returns ctx.Err() as soon as the client is gone.
// There is no time.Sleep and no goroutine behind it, so nothing can leak.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
