// Command nahook is the process entry point (design §6.8): it loads the
// configuration, starts the control socket and the data plane, and translates
// SIGHUP, SIGTERM and SIGINT into a reload and a graceful shutdown.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/ARYYDS/NaHook/internal/config"
	"github.com/ARYYDS/NaHook/internal/control"
	"github.com/ARYYDS/NaHook/internal/gate"
	"github.com/ARYYDS/NaHook/internal/obs"
	"github.com/ARYYDS/NaHook/internal/proxy"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run wires the process signals and hands them to runWithSignals, which is what
// the in-process tests drive directly: a Windows test process cannot deliver
// SIGHUP, and the shutdown path should not depend on signal delivery anyway.
func run(args []string, stdout, stderr io.Writer) int {
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigs)
	return runWithSignals(args, stdout, stderr, sigs)
}

// runWithSignals runs one nahook process until the control socket or the data
// plane stops, or until a signal asks it to. It returns the process exit code:
// 0 for a normal stop, 1 when startup fails (design §6.8).
//
// The logger handed to proxy.NewHandler and control.Serve is the same object,
// and slog.SetDefault is never called, so every event of both planes lands in
// one JSON stream on stdout and journald collects it.
func runWithSignals(args []string, stdout, stderr io.Writer, sigs <-chan os.Signal) int {
	fs := flag.NewFlagSet("nahook", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "/etc/nahook/config.json", "path to the configuration file")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "nahook: %v\n", err)
		return 1
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(stderr, "nahook: %v\n", err)
		return 1
	}

	store := config.NewStore(*configPath, cfg)
	log := obs.NewLogger(stdout)
	stats := obs.NewStats()
	gates := gate.NewRegistry()
	handler := proxy.NewHandler(store, gates, stats, log)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The control plane starts first (design §6.8). Serve only returns when the
	// socket could not be opened or when ctx ends.
	controlErr := make(chan error, 1)
	go func() {
		controlErr <- control.Serve(ctx, cfg.ControlSocket, store, gates, stats, log)
	}()

	// No ReadTimeout or WriteTimeout: a streaming response may legitimately
	// take as long as NewAPI needs (design §4).
	srv := &http.Server{Addr: cfg.Listen, Handler: handler}
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.ListenAndServe()
	}()

	for {
		select {
		case err := <-controlErr:
			if err != nil {
				log.Error("startup_failed", "component", "control", "error", err.Error())
				return 1
			}
			return 0
		case err := <-serveErr:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("startup_failed", "component", "data", "error", err.Error())
				return 1
			}
			return 0
		case sig := <-sigs:
			if sig == syscall.SIGHUP {
				// The signal has no peer, so there is no uid to record.
				if err := store.Reload(); err != nil {
					log.Error("config.reload_failed", "error", err.Error())
				} else {
					log.Info("config.reloaded", "uid", "-")
				}
				continue
			}
			// SIGTERM or SIGINT: stop accepting new control commands and let the
			// data plane drain. Shutdown has no deadline of its own; systemd's
			// TimeoutStopSec is the outer bound (design §6.8).
			cancel()
			_ = srv.Shutdown(context.Background())
			return 0
		}
	}
}
