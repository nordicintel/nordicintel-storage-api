package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nordicintel/nordicintel-storage-api/internal/apidocs"
	"github.com/nordicintel/nordicintel-storage-api/internal/config"
	"github.com/nordicintel/nordicintel-storage-api/internal/httpapi"
	"github.com/nordicintel/nordicintel-storage-api/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("API stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	configuration, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: configuration.LogLevel}))
	startupContext, startupCancel := context.WithTimeout(context.Background(), 15*time.Second)
	database, err := store.Open(startupContext, configuration.DatabaseURL, configuration.DBMaxConns)
	startupCancel()
	if err != nil {
		return fmt.Errorf("database startup check: %w", err)
	}
	defer database.Close()

	handler := httpapi.New(database, logger, httpapi.Options{
		ReadWriteToken: configuration.ReadWriteToken, ReadOnlyToken: configuration.ReadOnlyToken,
		MaxRequestBytes: configuration.MaxRequestBytes, MaxCells: configuration.MaxCells,
		RequestTimeout: configuration.RequestTimeout, DBTimeout: configuration.DBTimeout,
		OpenAPI: apidocs.Specification(), Docs: apidocs.Handler(),
	})
	server := &http.Server{
		Addr:              fmt.Sprintf("0.0.0.0:%d", configuration.Port),
		Handler:           handler.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       configuration.RequestTimeout,
		WriteTimeout:      configuration.RequestTimeout,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 * 1024,
	}

	stopContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serveErrors := make(chan error, 1)
	go func() {
		logger.Info("API ready", "port", configuration.Port)
		serveErrors <- server.ListenAndServe()
	}()

	select {
	case err := <-serveErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-stopContext.Done():
		logger.Info("shutdown started")
	}

	shutdownContext, cancel := context.WithTimeout(context.Background(), configuration.ShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownContext); err != nil {
		_ = server.Close()
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	logger.Info("shutdown complete")
	return nil
}
