// Command container-ops runs the central container dashboard.
//
//	container-ops -config /etc/container-ops/config.yaml
//	container-ops hash-password            # reads a password from stdin
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"container-ops/internal/auth"
	"container-ops/internal/config"
	"container-ops/internal/github"
	"container-ops/internal/harbor"
	"container-ops/internal/inventory"
	"container-ops/internal/web"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "hash-password" {
		hashPassword()
		return
	}
	cfgPath := flag.String("config", envOr("CONFIG_FILE", "config.yaml"), "path to the YAML config")
	flag.Parse()

	level := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})))

	if err := run(*cfgPath); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if cfg.Auth.Mode == "none" {
		slog.Warn("auth mode is none: anyone who can reach the dashboard can deploy and tear down")
	}
	gh := github.New(cfg.GitHub.APIURL, cfg.GitHub.Token, cfg.Server.RequestTimeout)
	if !gh.Enabled() {
		slog.Warn("GitHub token not set: env-var checks are unknown and deploy/teardown are disabled")
	}
	hb, err := harbor.New(cfg.Harbor.URL, cfg.Harbor.Username, cfg.Harbor.Password, cfg.Harbor.CAFile, cfg.Server.RequestTimeout)
	if err != nil {
		return fmt.Errorf("harbor: %w", err)
	}
	inv, err := inventory.New(cfg, gh, hb)
	if err != nil {
		return err
	}
	authn, err := auth.New(cfg.Auth, cfg.Server.SecretKey)
	if err != nil {
		return err
	}
	srv, err := web.New(cfg, inv, gh, authn)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go inv.Run(ctx)

	hs := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: log streams are long-lived. Each stream is capped
		// by the server itself (30 minutes, max_log_streams at once).
	}
	errCh := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.Server.Listen, "version", version, "projects", len(cfg.Projects))
		errCh <- hs.ListenAndServe()
	}()
	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return hs.Shutdown(shutdown)
}

func hashPassword() {
	fmt.Fprint(os.Stderr, "Password: ")
	pw, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && pw == "" {
		fmt.Fprintln(os.Stderr, "no password read")
		os.Exit(1)
	}
	pw = strings.TrimRight(pw, "\r\n")
	if len(pw) < 12 {
		fmt.Fprintln(os.Stderr, "use at least 12 characters")
		os.Exit(1)
	}
	h, err := auth.HashPassword(pw)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(h)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
