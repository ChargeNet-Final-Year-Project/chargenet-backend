// Command server is the ChargeNet backend entry point.
package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/chargenet/chargenet-backend/internal/api"
	"github.com/chargenet/chargenet-backend/internal/config"
	"github.com/chargenet/chargenet-backend/internal/store"
)

func main() {
	cfg := config.Load()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("server: cannot reach database: %v", err)
	}
	defer db.Close()

	if err := store.Migrate(ctx, db); err != nil {
		log.Fatalf("server: migrate failed: %v", err)
	}

	st := store.New(db)
	if err := st.ConfigureDemoNetworks(ctx, cfg.PublicBaseURL); err != nil {
		log.Fatalf("server: configure emulators: %v", err)
	}
	if err := st.SeedUsers(ctx); err != nil {
		log.Fatalf("server: seed users failed: %v", err)
	}
	if err := st.SeedPortalUsers(ctx); err != nil {
		log.Fatalf("server: seed portal users failed: %v", err)
	}

	apiServer := api.NewServer(cfg, st)

	// Open the socket before anything else starts. Run below boots the CPO
	// emulators, and each pushes its whole network the moment it does; a push
	// fired before this listener exists dies with "connection refused". Bound
	// first, the kernel queues that first push and Serve answers it.
	ln, err := net.Listen("tcp", ":"+cfg.Port)
	if err != nil {
		log.Fatalf("server: listen: %v", err)
	}
	log.Printf("server: listening on :%s (gateway=%s, simTimeScale=%d)", cfg.Port, cfg.Gateway, cfg.SimTimeScale)

	// No WriteTimeout: SSE streams stay open for the duration of a session.
	srv := &http.Server{
		Handler:           apiServer.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: serve: %v", err)
		}
	}()

	// Resume whatever the previous process left mid-charge, then start
	// consuming gateway telemetry. Run returns at once.
	apiServer.Run(ctx)

	<-ctx.Done()
	log.Println("server: shutting down")

	// End the session streams before draining: an SSE response never finishes on
	// its own, so a stream left open would make the graceful shutdown wait out
	// its whole timeout.
	apiServer.Close()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("server: graceful shutdown failed: %v", err)
	}
}
