// Command nahookctl drives the control socket of one nahook instance
// (design §6.5).
//
// Exit codes: 0 success, 2 bad arguments, 1 the control plane answered with an
// error or could not be reached.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// defaultSocket is the production control socket (design §6.5).
const defaultSocket = "/run/nahook/ctl.sock"

const usage = `usage: nahookctl [--socket PATH] <command>

commands:
  retry get [--model M]        show the retry ceiling
  retry set N [--model M]      set it for one model, or globally without --model
  retry unset --model M        drop the per-model value
  stats                        counters and gate states
  reload                       re-read the configuration file

exit codes: 0 success, 2 bad arguments, 1 control plane error
`

var errHelp = errors.New("help requested")

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// command is one parsed invocation.
type command struct {
	socket string
	model  string
	verb   string // get, set, unset, stats, reload
	value  int
}

// run is main without the process, so tests can drive it directly.
func run(args []string, stdout, stderr io.Writer) int {
	cmd, err := parseArgs(args)
	if errors.Is(err, errHelp) {
		fmt.Fprint(stdout, usage)
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "nahookctl: %v\n\n%s", err, usage)
		return 2
	}

	status, body, err := send(cmd)
	if err != nil {
		fmt.Fprintf(stderr, "nahookctl: %v\n", err)
		return 1
	}
	if status != http.StatusOK {
		fmt.Fprintf(stderr, "nahookctl: control returned %d: %s\n", status, body)
		return 1
	}
	if body != "" {
		fmt.Fprintln(stdout, body)
	}
	return 0
}

// parseArgs accepts flags before or after the positional words, because
// design §6.5 writes "retry set 5 --model gpt-6-astra".
func parseArgs(args []string) (command, error) {
	cmd := command{socket: defaultSocket}
	var positional []string

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-h", arg == "--help", arg == "help":
			return cmd, errHelp
		case arg == "--socket", arg == "-socket":
			if i+1 >= len(args) {
				return cmd, fmt.Errorf("%s needs a path", arg)
			}
			i++
			cmd.socket = args[i]
		case strings.HasPrefix(arg, "--socket="):
			cmd.socket = strings.TrimPrefix(arg, "--socket=")
		case arg == "--model", arg == "-model":
			if i+1 >= len(args) {
				return cmd, fmt.Errorf("%s needs a model name", arg)
			}
			i++
			cmd.model = args[i]
		case strings.HasPrefix(arg, "--model="):
			cmd.model = strings.TrimPrefix(arg, "--model=")
		case strings.HasPrefix(arg, "-") && arg != "-" && !isNumber(arg):
			return cmd, fmt.Errorf("unknown flag %s", arg)
		default:
			positional = append(positional, arg)
		}
	}

	if len(positional) == 0 {
		return cmd, errors.New("no command")
	}
	if cmd.socket == "" {
		return cmd, errors.New("--socket needs a path")
	}

	switch positional[0] {
	case "retry":
		if len(positional) < 2 {
			return cmd, errors.New("retry needs get, set or unset")
		}
		switch positional[1] {
		case "get":
			if len(positional) != 2 {
				return cmd, errors.New("retry get takes no arguments")
			}
			cmd.verb = "get"
		case "set":
			if len(positional) != 3 {
				return cmd, errors.New("retry set needs a number")
			}
			n, err := strconv.Atoi(positional[2])
			if err != nil {
				return cmd, fmt.Errorf("retry set %q: not a whole number", positional[2])
			}
			cmd.verb = "set"
			cmd.value = n
		case "unset":
			if len(positional) != 2 {
				return cmd, errors.New("retry unset takes no arguments")
			}
			if cmd.model == "" {
				return cmd, errors.New("retry unset needs --model")
			}
			cmd.verb = "unset"
		default:
			return cmd, fmt.Errorf("unknown retry command %q", positional[1])
		}
	case "stats":
		if len(positional) != 1 {
			return cmd, errors.New("stats takes no arguments")
		}
		cmd.verb = "stats"
	case "reload":
		if len(positional) != 1 {
			return cmd, errors.New("reload takes no arguments")
		}
		cmd.verb = "reload"
	default:
		return cmd, fmt.Errorf("unknown command %q", positional[0])
	}

	return cmd, nil
}

// isNumber reports whether arg is an integer literal, so a negative value such
// as "-1" reaches the control plane (which answers 400) instead of being taken
// for an unknown flag.
func isNumber(arg string) bool {
	_, err := strconv.Atoi(arg)
	return err == nil
}

// send performs the control-plane call over the unix socket.
func send(cmd command) (int, string, error) {
	req, err := buildRequest(cmd)
	if err != nil {
		return 0, "", err
	}

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", cmd.socket)
		},
	}}

	resp, err := client.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("control socket %s: %w", cmd.socket, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, "", fmt.Errorf("read response: %w", err)
	}
	return resp.StatusCode, strings.TrimSpace(string(body)), nil
}

func buildRequest(cmd command) (*http.Request, error) {
	const base = "http://nahook"

	switch cmd.verb {
	case "get":
		target := base + "/v1/retry"
		if cmd.model != "" {
			target += "?model=" + url.QueryEscape(cmd.model)
		}
		return http.NewRequest(http.MethodGet, target, nil)
	case "set":
		scope := cmd.model
		if scope == "" {
			scope = "*"
		}
		body, err := json.Marshal(struct {
			Scope string `json:"scope"`
			N     int    `json:"n"`
		}{Scope: scope, N: cmd.value})
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		req, err := http.NewRequest(http.MethodPut, base+"/v1/retry", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		return req, nil
	case "unset":
		return http.NewRequest(http.MethodDelete, base+"/v1/retry?model="+url.QueryEscape(cmd.model), nil)
	case "stats":
		return http.NewRequest(http.MethodGet, base+"/v1/stats", nil)
	case "reload":
		return http.NewRequest(http.MethodPost, base+"/v1/reload", nil)
	default:
		return nil, fmt.Errorf("unknown command %q", cmd.verb)
	}
}
