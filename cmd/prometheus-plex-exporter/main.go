package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	kitlog "github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/drewstreib/prometheus-plex-exporter/pkg/metrics"
	"github.com/drewstreib/prometheus-plex-exporter/pkg/plex"
)

const (
	MetricsServerAddr = ":9000"
)

var (
	log = kitlog.NewLogfmtLogger(kitlog.NewSyncWriter(os.Stderr))
)

// silenceClientChatter points os.Stdout at /dev/null.
//
// The plex client logs with unconditional fmt.Print* to STDOUT and exposes no toggle: it
// dumps every websocket message raw, plus a line per unknown event type and per unmarshal
// failure. Measured 2026-08-28 in Loki: 163,405 of this pod's 166,207 lines (98.3%) were
// that chatter, against 2,800 of our own, making the exporter one of the cluster's top log
// producers for no information. All 20 of the client's stdout writes are either debug
// chatter or an error it ALSO returns (or hands to the SubscribeToNotifications error
// callback), and we log those ourselves — so nothing is lost.
//
// ⚠ This binary therefore logs on STDERR ONLY. Set PLEX_CLIENT_DEBUG to any non-empty
// value to get the chatter back; that is the only way to see the raw notification stream.
func silenceClientChatter() {
	if os.Getenv("PLEX_CLIENT_DEBUG") != "" {
		return
	}

	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		// Never fatal: the chatter is noise, not a dependency.
		level.Warn(log).Log("msg", "cannot discard plex client chatter", "path", os.DevNull, "err", err)
		return
	}

	// Deliberately never closed: it must outlive every goroutine that writes to it.
	os.Stdout = devNull
}

func main() {
	silenceClientChatter()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	serverAddress := os.Getenv("PLEX_SERVER")
	if serverAddress == "" {
		level.Error(log).Log("msg", "PLEX_SERVER environment variable must be specified")
		os.Exit(1)
	}

	plexToken := os.Getenv("PLEX_TOKEN")
	if plexToken == "" {
		level.Error(log).Log("msg", "PLEX_TOKEN environment variable must be specified")
		os.Exit(1)
	}

	server, err := plex.NewServer(serverAddress, plexToken)
	if err != nil {
		level.Error(log).Log("msg", "cannot initialize connection to plex server", "error", err)
		os.Exit(1)
	}

	metrics.Register(server)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	metricsServer := http.Server{
		Addr:         MetricsServerAddr,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		level.Info(log).Log("msg", "starting metrics server on "+MetricsServerAddr)
		err = metricsServer.ListenAndServe()
		if !errors.Is(err, http.ErrServerClosed) {
			level.Error(log).Log("msg", "cannot start metrics server", "error", err)
		}
	}()

	exitCode := 0
	err = server.Listen(ctx, log)
	if err != nil {
		level.Error(log).Log("msg", "cannot listen to plex server events", "error", err)
		exitCode = 1
	}

	level.Debug(log).Log("msg", "shutting down metrics server")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	if err := metricsServer.Shutdown(shutdownCtx); err != nil {
		level.Error(log).Log("msg", "cannot gracefully shutdown metrics server", "error", err)
	}

	os.Exit(exitCode)
}
