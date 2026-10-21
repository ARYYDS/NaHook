// Package startup reads the bounded startup phase of a streaming upstream
// response and decides whether it carried an upstream rate-limit error
// (design §1.3, interface 2.1).
//
// It is a port of H3 relay/channel/openai/responses_startup_error.go at
// 38e69e3001606d8cdf7de6c4b872d8329b405c25. The difference is deliberate: every
// abnormal case (buffer limit, event limit, invalid JSON, EOF, read error)
// releases the stream instead of turning it into a 502, because releasing is
// never worse than the official behaviour (design §8).
package startup

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

const (
	// MaxBytes is the largest startup prefix H3 is willing to buffer
	// (responses_startup_error.go:24).
	MaxBytes = 256 << 10
	// MaxEvents is the largest number of startup events H3 is willing to
	// inspect (responses_startup_error.go:25).
	MaxEvents = 128
)

// Verdict is what Read decided about the startup phase.
type Verdict int

const (
	// VerdictRelease means "write Prefix to the client and keep copying Rest".
	VerdictRelease Verdict = iota
	// VerdictRateLimit means the startup phase carried an upstream rate limit;
	// the caller closes the body and retries or answers 429 (design §1.6).
	VerdictRateLimit
)

// UpstreamError carries the upstream error fields as they will be echoed in the
// 429 body (design §1.4). Code keeps the upstream JSON value verbatim: it may be
// a string or a number, and proxy429 writes it back unchanged.
type UpstreamError struct {
	Message string
	Type    string
	Param   string
	Code    json.RawMessage
}

// Result is one startup read. Prefix plus Rest always equals the bytes the
// upstream sent, so releasing reconstructs the stream exactly (interface §3).
type Result struct {
	Verdict Verdict
	Prefix  []byte
	Rest    *bufio.Reader
	Err     UpstreamError

	// HeartbeatOnly reports that the deciding frame was preceded by at least one
	// event and that every one of them consisted of comment lines only
	// (": PING"), so no upstream data had arrived yet. It is only meaningful for
	// VerdictRateLimit (design §1.3).
	HeartbeatOnly bool
	// Untyped reports that the deciding frame carried neither a type field nor
	// an event: line. It is only meaningful for VerdictRateLimit (design §1.3).
	Untyped bool
}

// decision is the three-way outcome of inspecting one event.
type decision int

const (
	decisionContinue decision = iota
	decisionRelease
	decisionRateLimit
)

// frame is the startup event shape from design §1.3, mirroring
// responsesStartupFrame in H3. RawMessage is used for the fields whose JSON
// value type is not fixed, because a JSON null must read as absent.
type frame struct {
	Type    string          `json:"type"`
	Code    json.RawMessage `json:"code"`
	Message string          `json:"message"`
	Param   json.RawMessage `json:"param"`
	Error   json.RawMessage `json:"error"`
	// Response keeps the name and shape H3 inspects. Status stays raw like
	// H3's OpenAIResponsesResponse.Status, so a non-string status cannot make
	// the whole frame unparseable; upstreamError reads it for the failed
	// fallback (design §1.3).
	Response *frameResponse `json:"response"`
}

type frameResponse struct {
	Status json.RawMessage `json:"status"`
	Error  json.RawMessage `json:"error"`
}

// Read consumes the startup phase of a streaming response. It stops at the first
// rate-limit error (VerdictRateLimit), at the first event that ends the startup
// phase (VerdictRelease, with that event in Prefix), or at any abnormal
// condition (VerdictRelease).
func Read(body io.Reader) Result {
	reader := bufio.NewReader(body)
	var prefix, event bytes.Buffer
	var dataLines []string
	var eventName string
	events := 0
	commentOnly := true // every non-empty line of the current event is a comment
	heartbeats := 0     // comment-only events seen before the deciding frame
	dataSeen := false   // an earlier event carried more than comments

	for {
		raw, overflow, readErr := readLine(reader, MaxBytes-prefix.Len()-event.Len())
		if overflow {
			return Result{
				Verdict: VerdictRelease,
				Prefix:  concat(&prefix, &event, string(raw)),
				Rest:    reader,
			}
		}
		line := string(raw)
		if len(line) > 0 {
			event.WriteString(line)

			trimmed := strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(trimmed, "event:"):
				eventName = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
				commentOnly = false
			case strings.HasPrefix(trimmed, "data:"):
				dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
				commentOnly = false
			case strings.HasPrefix(trimmed, ":"):
				// A comment frame such as ": PING". It stays in the event buffer
				// and therefore in Prefix, which is how keep-alives survive
				// (interface 2.5: 注释帧). It does not end the comment-only
				// state, so the event only counts as a heartbeat if every line
				// looks like this one.
			case trimmed != "":
				// Some other non-empty line still means upstream data started.
				commentOnly = false
			}
		}

		if strings.TrimSpace(line) == "" || readErr == io.EOF {
			if event.Len() > 0 {
				events++
				if events > MaxEvents {
					return Result{
						Verdict: VerdictRelease,
						Prefix:  concat(&prefix, &event, ""),
						Rest:    reader,
					}
				}

				d, e, typed := inspect(eventName, strings.Join(dataLines, "\n"))
				prefix.Write(event.Bytes())
				switch d {
				case decisionRelease:
					return Result{Verdict: VerdictRelease, Prefix: prefix.Bytes(), Rest: reader}
				case decisionRateLimit:
					return Result{
						Verdict:       VerdictRateLimit,
						Prefix:        prefix.Bytes(),
						Rest:          reader,
						Err:           e,
						HeartbeatOnly: heartbeats > 0 && !dataSeen,
						Untyped:       !typed,
					}
				case decisionContinue:
					if commentOnly {
						heartbeats++
					} else {
						dataSeen = true
					}
				}
			}
			event.Reset()
			dataLines = nil
			eventName = ""
			commentOnly = true
		}

		if readErr != nil {
			// EOF or a read error: release what has been read, including an
			// unterminated last event, so Prefix+Rest still reconstructs the
			// stream (interface §3).
			return Result{Verdict: VerdictRelease, Prefix: concat(&prefix, &event, ""), Rest: reader}
		}
	}
}

// inspect classifies one complete event (design §1.3). The third result reports
// whether the frame was typed: it carried a type field or an event: line. The
// proxy needs that for NewAPI's terminal error after a heartbeat (design §1.6).
func inspect(eventName, data string) (decision, UpstreamError, bool) {
	if data == "" {
		// A comment-only event: nothing to decide and no type either.
		return decisionContinue, UpstreamError{}, false
	}

	var f frame
	if err := json.Unmarshal([]byte(data), &f); err != nil {
		// For example "[DONE]": release, do not invent an error.
		return decisionRelease, UpstreamError{}, false
	}

	eventType := strings.TrimSpace(f.Type)
	if eventType == "" {
		eventType = strings.TrimSpace(eventName)
	}
	typed := eventType != ""

	if e, ok := upstreamError(eventType, &f); ok {
		if IsRateLimit(e) {
			return decisionRateLimit, e, typed
		}
		return decisionRelease, UpstreamError{}, typed
	}

	if isStartupEvent(eventType) {
		return decisionContinue, UpstreamError{}, typed
	}
	// The first non-startup event ends the startup phase. A Chat Completions
	// chunk carries no type at all, so it lands here too.
	return decisionRelease, UpstreamError{}, typed
}

// isStartupEvent reports whether eventType belongs to the startup set
// (responses_startup_error.go:196).
func isStartupEvent(eventType string) bool {
	switch strings.ToLower(strings.TrimSpace(eventType)) {
	case "response.created", "response.in_progress", "response.queued":
		return true
	default:
		return false
	}
}

// readLine reads one line, the trailing '\n' included, but stops as soon as it
// holds more than limit bytes (overflow). Unlike bufio.Reader.ReadString it
// never buffers an unbounded line, so an upstream that sends a huge line
// without a newline cannot exhaust memory before MaxBytes is enforced (design
// §1.3). Every byte it consumed is in line.
func readLine(r *bufio.Reader, limit int) (line []byte, overflow bool, err error) {
	for {
		frag, e := r.ReadSlice('\n')
		line = append(line, frag...)
		if len(line) > limit {
			return line, true, nil
		}
		if e != bufio.ErrBufferFull {
			return line, false, e
		}
	}
}

// concat builds the released prefix without aliasing either buffer.
func concat(prefix, event *bytes.Buffer, tail string) []byte {
	out := make([]byte, 0, prefix.Len()+event.Len()+len(tail))
	out = append(out, prefix.Bytes()...)
	out = append(out, event.Bytes()...)
	out = append(out, tail...)
	return out
}
