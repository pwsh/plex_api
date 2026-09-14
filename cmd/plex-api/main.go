// Command plex-api serves the Plex Media Server library databases over HTTP by
// driving Plex's bundled SQLite shell as a subprocess.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pwsh/plex_api/internal/api"
	"github.com/pwsh/plex_api/internal/config"
	"github.com/pwsh/plex_api/internal/indexes"
)

// Version is the service version; override at build time with
// -ldflags "-X main.Version=…".
var Version = "0.1.0"

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	ensureIndexes := flag.Bool("ensure-indexes", false,
		"create the managed SQLite indexes if PLEX_API_INDEXES is set, then exit; run with Plex stopped")
	dropIndexes := flag.Bool("drop-indexes", false,
		"drop the managed SQLite indexes and the state file, then exit; run with Plex stopped")
	flag.Parse()
	if *showVersion {
		fmt.Println("plex-api", Version)
		return
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("configuration: %v", err)
	}

	// CLI modes. Both are one-shot, never start the HTTP server, and exit 1
	// on failure without touching the state file, so the s6 oneshot can log
	// the problem and let Plex start regardless.
	if *ensureIndexes || *dropIndexes {
		run := indexes.Ensure
		if *dropIndexes {
			run = indexes.DropAll
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if err := run(ctx, cfg, log.Printf); err != nil {
			log.Printf("[plex-api] index step failed: %v", err)
			os.Exit(1)
		}
		return
	}

	srv := api.New(cfg, Version)
	httpSrv := &http.Server{
		Addr:              cfg.Bind,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Requests can be as slow as the query timeout allows.
		WriteTimeout: cfg.QueryTimeout + 30*time.Second,
		IdleTimeout:  120 * time.Second,
	}

	log.Printf("plex-api %s listening on %s", Version, cfg.Bind)
	log.Printf("shell=%q db_dir=%q write=%v write_while_running=%v auth=%v",
		cfg.ShellPath(), cfg.DBDir, cfg.AllowWrite, cfg.WriteWhileRunning, cfg.Token != "")

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		log.Fatalf("listen: %v", err)
	case sig := <-sigCh:
		log.Printf("received %s, shutting down", sig)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	defer srv.Close()
	if err := httpSrv.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
		os.Exit(1)
	}
	log.Print("stopped")
}
