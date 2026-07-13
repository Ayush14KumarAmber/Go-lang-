// Command server is the entrypoint for the Ethereum Blockchain Explorer
// API. It wires together configuration, logging, the Gin HTTP server, and
// (in later milestones) the Ethereum client and Redis cache, then serves
// requests until it receives SIGINT/SIGTERM, at which point it drains
// in-flight requests before exiting.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"eth-explorer/internal/api/handlers"
	"eth-explorer/internal/api/routes"
	"eth-explorer/internal/config"
	"eth-explorer/internal/logger"
)

// version is a build-time constant today; a later milestone wires this to
// -ldflags "-X main.version=..." from the CI pipeline.
const version = "0.1.0"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	log, err := logger.New(cfg.Log.Level, cfg.Server.Environment)
	if err != nil {
		return fmt.Errorf("initializing logger: %w", err)
	}
	defer func() { _ = log.Sync() }()

	log.Info("starting eth-explorer",
		zap.String("version", version),
		zap.String("environment", cfg.Server.Environment),
	)

	if strings.EqualFold(cfg.Server.Environment, "production") {
		gin.SetMode(gin.ReleaseMode)
	}

	router := gin.New()

	healthHandler := handlers.NewHealthHandler(version)
	routes.Register(router, routes.Dependencies{
		HealthHandler: healthHandler,
	})

	srv := &http.Server{
		Addr:         ":" + cfg.Server.Port,
		Handler:      router,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}

	serverErrs := make(chan error, 1)
	go func() {
		log.Info("http server listening", zap.String("addr", srv.Addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrs <- err
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serverErrs:
		return fmt.Errorf("server error: %w", err)
	case sig := <-quit:
		log.Info("shutdown signal received", zap.String("signal", sig.String()))
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("graceful shutdown failed: %w", err)
	}

	log.Info("server shut down cleanly")
	return nil
}
