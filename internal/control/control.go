// Package control serves the control socket (design §5, §6.4, interface 2.6).
//
// Serve writes the log lines of design §6.4 to the logger it is given;
// cmd/nahook passes the same obs.NewLogger(os.Stdout) logger it hands to
// proxy.NewHandler, so no package depends on slog.Default.
package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/ARYYDS/NaHook/internal/config"
	"github.com/ARYYDS/NaHook/internal/gate"
	"github.com/ARYYDS/NaHook/internal/obs"
)

const (
	// dirSyncWarning is what design §6.4 puts into the body when the change is
	// already in effect but the directory fsync failed.
	dirSyncWarning = "applied; directory fsync failed, not durable across power loss"

	requestBodyLimit = 1 << 20
	shutdownTimeout  = 5 * time.Second
)

// Serve listens on the control socket and answers until ctx is done. It removes
// a stale socket file first, then chmods the new one to 0600. log must not be
// nil.
func Serve(ctx context.Context, socket string, store *config.Store, gates *gate.Registry, stats *obs.Stats, log *slog.Logger) error {
	if err := os.Remove(socket); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("control: remove stale socket %s: %w", socket, err)
	}

	ln, err := net.Listen("unix", socket)
	if err != nil {
		return fmt.Errorf("control: listen on %s: %w", socket, err)
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		_ = ln.Close()
		return fmt.Errorf("control: chmod %s: %w", socket, err)
	}

	h := &handler{store: store, gates: gates, stats: stats, log: log}
	srv := &http.Server{
		Handler: h.mux(),
		ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
			return context.WithValue(ctx, uidKey{}, peerUID(conn))
		},
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		_ = os.Remove(socket)
		return nil
	}
}

type handler struct {
	store *config.Store
	gates *gate.Registry
	stats *obs.Stats
	log   *slog.Logger
}

func (h *handler) mux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/retry", h.getRetry)
	mux.HandleFunc("PUT /v1/retry", h.putRetry)
	mux.HandleFunc("DELETE /v1/retry", h.deleteRetry)
	mux.HandleFunc("GET /v1/stats", h.getStats)
	mux.HandleFunc("POST /v1/reload", h.reload)
	return mux
}

// GET /v1/retry?model=M → {"scope","max","source"} (design §5).
func (h *handler) getRetry(w http.ResponseWriter, r *http.Request) {
	model := r.URL.Query().Get("model")
	if model == "" {
		writeJSON(w, http.StatusOK, retryView{
			Scope:  "*",
			Max:    h.store.RetryMax(""),
			Source: "global",
		})
		return
	}

	source := "global"
	if _, ok := h.store.Current().Retry.PerModel[model]; ok {
		source = "per_model"
	}
	writeJSON(w, http.StatusOK, retryView{
		Scope:  model,
		Max:    h.store.RetryMax(model),
		Source: source,
	})
}

// PUT /v1/retry {"scope","n"} → {"scope","old","new"}.
func (h *handler) putRetry(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Scope string `json:"scope"`
		N     *int   `json:"n"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, requestBodyLimit)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.N == nil {
		writeError(w, http.StatusBadRequest, "n is required")
		return
	}

	old, err := h.store.SetRetryMax(req.Scope, *req.N)
	h.logRetryChange(r.Context(), req.Scope, old, *req.N, err)
	writeRetryChange(w, req.Scope, old, *req.N, err)
}

// DELETE /v1/retry?model=M → {"scope","removed"}.
func (h *handler) deleteRetry(w http.ResponseWriter, r *http.Request) {
	model := r.URL.Query().Get("model")
	if model == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}

	old := h.store.RetryMax(model)
	err := h.store.UnsetRetryMax(model)
	// After the unset the model falls back to the global value; that is the new
	// value the log reports.
	h.logRetryChange(r.Context(), model, old, h.store.RetryMax(model), err)
	writeRemoved(w, model, err)
}

// GET /v1/stats → {"counters":{…},"gates":[…]}.
func (h *handler) getStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, struct {
		Counters map[string]int64 `json:"counters"`
		Gates    []gate.GateStat  `json:"gates"`
	}{
		Counters: h.stats.Snapshot(),
		Gates:    h.gates.Snapshot(),
	})
}

// POST /v1/reload → {"ok":true}; failures keep the old snapshot (design §6.4).
func (h *handler) reload(w http.ResponseWriter, r *http.Request) {
	if err := h.store.Reload(); err != nil {
		h.log.Error("config.reload_failed", "error", err.Error())
		writeStoreError(w, err)
		return
	}
	h.log.Info("config.reloaded", "uid", uidFrom(r.Context()))
	writeJSON(w, http.StatusOK, struct {
		OK bool `json:"ok"`
	}{OK: true})
}

// logRetryChange writes config.retry_max_changed for a change that took effect,
// which includes the ErrDirSync case (design §6.4).
func (h *handler) logRetryChange(ctx context.Context, scope string, old, newValue int, err error) {
	if err != nil && !errors.Is(err, config.ErrDirSync) {
		return
	}
	h.log.Info("config.retry_max_changed",
		"scope", scope, "old", old, "new", newValue, "uid", uidFrom(ctx))
}

// writeRetryChange answers a SetRetryMax result.
func writeRetryChange(w http.ResponseWriter, scope string, old, newValue int, err error) {
	view := retryChange{Scope: scope, Old: old, New: newValue}
	switch {
	case err == nil:
	case errors.Is(err, config.ErrDirSync):
		// The file already holds the new value, so this is a success with a
		// durability warning.
		view.Warning = dirSyncWarning
	default:
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// writeRemoved answers an UnsetRetryMax result.
func writeRemoved(w http.ResponseWriter, scope string, err error) {
	view := removedView{Scope: scope, Removed: true}
	switch {
	case err == nil:
	case errors.Is(err, config.ErrDirSync):
		view.Warning = dirSyncWarning
	default:
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// writeStoreError maps a config error to the status code of design §6.4.
func writeStoreError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, config.ErrInvalidRetry), errors.Is(err, config.ErrInvalidConfig):
		status = http.StatusBadRequest
	case errors.Is(err, config.ErrRetryNotSet):
		status = http.StatusNotFound
	case errors.Is(err, config.ErrRestartRequired):
		status = http.StatusConflict
	}
	writeError(w, status, err.Error())
}

type retryView struct {
	Scope  string `json:"scope"`
	Max    int    `json:"max"`
	Source string `json:"source"`
}

type retryChange struct {
	Scope   string `json:"scope"`
	Old     int    `json:"old"`
	New     int    `json:"new"`
	Warning string `json:"warning,omitempty"`
}

type removedView struct {
	Scope   string `json:"scope"`
	Removed bool   `json:"removed"`
	Warning string `json:"warning,omitempty"`
}

type errorBody struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errorBody{Error: message})
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

type uidKey struct{}

// uidFrom returns the peer uid recorded by ConnContext, or "-" when the
// platform has no SO_PEERCRED.
func uidFrom(ctx context.Context) string {
	if v, ok := ctx.Value(uidKey{}).(string); ok && v != "" {
		return v
	}
	return "-"
}
