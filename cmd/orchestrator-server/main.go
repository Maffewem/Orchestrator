// Command orchestrator-server runs the Orchestrator control plane and dashboard.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Maffewem/Orchestrator/internal/server"
	"github.com/Maffewem/Orchestrator/internal/store"
	"github.com/Maffewem/Orchestrator/web"
)

var version = "dev"

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	addr := flag.String("addr", env("ORCH_ADDR", ":8080"), "listen address")
	dbPath := flag.String("db", env("ORCH_DB", "orchestrator.db"), "SQLite database path")
	interval := flag.Duration("heartbeat", 5*time.Second, "agent heartbeat interval")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Error("open database", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	srv, adminToken, err := server.New(ctx, st, server.Config{
		HeartbeatInterval: *interval,
		AdminToken:        os.Getenv("ORCH_ADMIN_TOKEN"),
		UI:                web.Dist(),
		Version:           version,
		Logger:            log,
	})
	if err != nil {
		log.Error("start server", "err", err)
		os.Exit(1)
	}
	if adminToken != "" {
		fmt.Fprintf(os.Stderr, "\n  Admin token (shown once, use it to sign in to the dashboard):\n\n    %s\n\n", adminToken)
	}

	go srv.Run(ctx)
	hs := &http.Server{Addr: *addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		hs.Shutdown(shutdown)
	}()
	log.Info("orchestrator listening", "addr", *addr, "version", version)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}
