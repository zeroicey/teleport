// Command teleport is the report-publishing backend.
//
// It serves the agent ingestion API, the private dashboard API and the
// server-rendered public share page. It is designed to run behind Caddy on a
// small VPS, listening only on a private interface.
//
// Usage:
//
//	teleport                  start the server
//	teleport hash-password    read a password on stdin and print ADMIN_PASSWORD_HASH
//	teleport migrate          apply pending migrations and exit
//	teleport version          print the build version
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/zeroicey/teleport/backend/internal/api"
	"github.com/zeroicey/teleport/backend/internal/config"
	"github.com/zeroicey/teleport/backend/internal/password"
	"github.com/zeroicey/teleport/backend/internal/store"
	"github.com/zeroicey/teleport/backend/internal/webui"
)

// version is overridable at build time with
// -ldflags "-X main.version=<git describe>".
var version = "dev"

func main() {
	// Subcommands are handled before config loading so `hash-password` works on
	// a machine that has no secrets configured yet.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "hash-password":
			if err := runHashPassword(); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
			return
		case "version", "--version", "-v":
			// `version --frontend` reports whether a frontend build is embedded,
			// which is the fastest way to confirm a deployment is the combined
			// binary and not an API-only one. It stays a separate flag rather
			// than extra output so existing scripts parsing `version` keep
			// working unchanged.
			if len(os.Args) > 2 && os.Args[2] == "--frontend" {
				if _, ok := webui.FS(); ok {
					fmt.Println("embedded")
				} else {
					fmt.Println("none")
				}
				return
			}
			fmt.Println(version)
			return
		}
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		os.Exit(1)
	}

	setupLogging(cfg.Environment)

	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		if err := ensureParentDir(cfg.DBPath); err != nil {
			fatal("prepare database directory", err)
		}
		db, err := store.Open(cfg.DBPath)
		if err != nil {
			fatal("open database", err)
		}
		defer db.Close()
		slog.Info("migrations up to date", "db", cfg.DBPath)
		return
	}

	if err := ensureParentDir(cfg.DBPath); err != nil {
		fatal("prepare database directory", err)
	}

	db, err := store.Open(cfg.DBPath)
	if err != nil {
		fatal("open database", err)
	}
	defer db.Close()
	slog.Info("database ready", "path", cfg.DBPath)

	// Warn about the one configuration mistake that silently weakens the
	// deployment: a non-Secure cookie on a public HTTPS origin, or the reverse.
	if cfg.Environment == "production" && !cfg.CookieSecure {
		slog.Warn("COOKIE_SECURE is false in production; the session cookie can be sent over plain HTTP")
	}

	handler, err := api.New(cfg, store.New(db))
	if err != nil {
		fatal("build http handler", err)
	}

	server := &http.Server{
		Addr:    cfg.Addr,
		Handler: handler,
		// Bound the header read so a slow-loris client cannot hold a connection
		// open indefinitely; there is no timeout on the body because a large
		// report upload is legitimate.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(slog.Default().Handler(), slog.LevelError),
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("listening",
			"addr", cfg.Addr,
			"prefix", cfg.RoutePrefix,
			"appBaseUrl", cfg.AppBaseURL(),
			"staticDir", cfg.StaticDir,
			"environment", cfg.Environment,
			"version", version,
		)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		fatal("server error", err)
	case sig := <-stop:
		slog.Info("shutting down", "signal", sig.String())
	}

	// Give in-flight requests a chance to finish before exiting.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		slog.Error("graceful shutdown failed", "error", err)
	}
	slog.Info("stopped")
}

// runHashPassword reads a password from stdin (or the first argument) and
// prints a ready-to-use ADMIN_PASSWORD_HASH line.
//
// Reading from stdin rather than an argument is the default because arguments
// leak into shell history and the process table.
func runHashPassword() error {
	var plaintext string
	if len(os.Args) > 2 {
		plaintext = os.Args[2]
	} else {
		fmt.Fprint(os.Stderr, "Password: ")
		reader := bufio.NewReader(os.Stdin)
		line, err := reader.ReadString('\n')
		if err != nil && line == "" {
			return fmt.Errorf("read password: %w", err)
		}
		plaintext = strings.TrimRight(line, "\r\n")
	}
	if plaintext == "" {
		return errors.New("password must not be empty")
	}

	hash, err := password.Hash(plaintext, password.DefaultIterations)
	if err != nil {
		return err
	}
	fmt.Println("ADMIN_PASSWORD_HASH=" + hash)
	return nil
}

func setupLogging(environment string) {
	level := slog.LevelInfo
	if environment == "development" {
		level = slog.LevelDebug
	}
	// Text output keeps `journalctl -u teleport` readable; JSON would be better
	// for a log pipeline, which this deployment does not have.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
}

// ensureParentDir creates the directory holding the database file. The schema
// itself is created by the migration runner.
func ensureParentDir(dbPath string) error {
	if dbPath == ":memory:" || strings.HasPrefix(dbPath, "file:") {
		return nil
	}
	dir := filepath.Dir(dbPath)
	if dir == "." || dir == "" {
		return nil
	}
	return os.MkdirAll(dir, 0o750)
}

func fatal(msg string, err error) {
	slog.Error(msg, "error", err)
	os.Exit(1)
}
