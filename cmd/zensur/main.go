package main

import (
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/sglre6355/zensur/internal/bot"
)

// version is set at build time via ldflags:
// go build -ldflags "-X main.version=1.0.0" ./cmd/zensur
var version = "dev"

func main() {
	// Configure JSON logging with default level
	logLevel := &slog.LevelVar{}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	})))

	// Load configuration
	cfg, err := bot.LoadConfig()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// Apply configured log level
	logLevel.Set(cfg.LogLevel)

	slog.Info("starting zensur", "version", version)

	// Create and start bot
	b := bot.NewBot(cfg)
	if err := b.Start(); err != nil {
		slog.Error("failed to start bot", "error", err)
		os.Exit(1)
	}

	// Wait for shutdown signal
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	slog.Info("received termination signal, shutting down")
	if err := b.Stop(); err != nil {
		slog.Error("failed to shutdown", "error", err)
	}

	slog.Info("completed bot shutdown")
	os.Exit(0)
}
