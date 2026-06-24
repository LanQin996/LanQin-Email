package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	smtpserver "github.com/emersion/go-smtp"

	"lanqin-email-api/internal/app"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := app.LoadConfig()

	svc, err := app.New(cfg, logger)
	if err != nil {
		logger.Error("failed to initialize app", "error", err)
		os.Exit(1)
	}
	defer svc.Close()

	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           svc.Router(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	submissionServers := &app.SubmissionServers{}
	if strings.TrimSpace(cfg.SubmissionAddr) != "" || strings.TrimSpace(cfg.SubmissionTLSAddr) != "" {
		tlsConfig, err := app.LoadServerTLSConfig(cfg)
		if err != nil {
			logger.Error("failed to initialize TLS config", "error", err)
			os.Exit(1)
		}
		submissionServers = svc.NewSubmissionServers(tlsConfig)
	}

	go func() {
		logger.Info("LanQin API listening", "addr", cfg.Addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server stopped unexpectedly", "error", err)
			os.Exit(1)
		}
	}()
	if submissionServers.Plain != nil {
		go func() {
			logger.Info("LanQin SMTP submission listening", "addr", cfg.SubmissionAddr)
			if err := submissionServers.Plain.ListenAndServe(); err != nil && !errors.Is(err, smtpserver.ErrServerClosed) {
				logger.Error("smtp submission server stopped unexpectedly", "error", err)
				os.Exit(1)
			}
		}()
	}
	if submissionServers.TLS != nil {
		go func() {
			logger.Info("LanQin SMTP implicit TLS submission listening", "addr", cfg.SubmissionTLSAddr)
			if err := submissionServers.TLS.ListenAndServeTLS(); err != nil && !errors.Is(err, smtpserver.ErrServerClosed) {
				logger.Error("smtp tls submission server stopped unexpectedly", "error", err)
				os.Exit(1)
			}
		}()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("server shutdown failed", "error", err)
		os.Exit(1)
	}
	if err := submissionServers.Shutdown(shutdownCtx); err != nil {
		logger.Error("smtp submission shutdown failed", "error", err)
		os.Exit(1)
	}
	logger.Info("server stopped")
}
