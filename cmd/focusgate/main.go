// Package main provides the entry point and CLI commands for the FocusGate daemon.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"focusgate/internal/app"
	"focusgate/internal/config"
	"focusgate/internal/ingest"
	"focusgate/internal/store"
	"focusgate/internal/update"
	"focusgate/internal/version"
	"focusgate/internal/web"
)

func main() {
	// Keep Go runtime GC aggressive so daemon RSS stays tiny on low-RAM hosts.
	debug.SetMemoryLimit(32 << 20)

	if len(os.Args) < 2 {
		if err := runServe(nil); err != nil {
			log.Fatal(err)
		}
		return
	}

	sub := os.Args[1]
	args := os.Args[2:]
	var err error
	switch sub {
	case "serve":
		err = runServe(args)
	case "check":
		err = runCheck(args)
	case "digest":
		err = runDigest(args)
	case "mode":
		err = runMode(args)
	case "simulate":
		err = runSimulate(args)
	case "version", "-v", "--version":
		runVersion()
		return
	case "update":
		err = runUpdate(args)
	case "-h", "--help", "help":
		printUsage()
		return
	default:
		printUsage()
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func printUsage() {
	fmt.Println(`FocusGate — Low-RAM focus-aware notification & feed router

Commands:
  serve [--config path]                     Start daemon, pollers, and embedded PWA
  check [--config path]                     Run one poll + classification pass and exit
  digest [--config path]                    Send queued digest items immediately
  mode <normal|deep_work|dnd> [--config]    Update persisted focus mode
  simulate --title "..." [options]          Dry-run routing without saving or sending
  update [--check] [--repo owner/repo]      Check or install in-place updates from GitHub Releases
  version                                   Print version and system information`)
}

func resolveConfigPath(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if _, err := os.Stat("focusgate.yaml"); err == nil {
		return "focusgate.yaml"
	}
	return ""
}

func bootstrap(ctx context.Context, cfgPath string) (config.Config, *store.Store, *app.Engine, error) {
	cfg, err := config.Load(resolveConfigPath(cfgPath))
	if err != nil {
		return config.Config{}, nil, nil, err
	}
	st, err := store.Open(cfg.Storage.Path)
	if err != nil {
		return config.Config{}, nil, nil, err
	}
	engine, err := app.NewEngine(ctx, cfg, st)
	if err != nil {
		_ = st.Close()
		return config.Config{}, nil, nil, err
	}
	return cfg, st, engine, nil
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgFlag := fs.String("config", "", "Path to focusgate.yaml")
	_ = fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, st, engine, err := bootstrap(ctx, *cfgFlag)
	if err != nil {
		return err
	}
	defer st.Close()

	srv, err := web.NewServer(engine)
	if err != nil {
		return err
	}

	httpServer := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	engine.StartBackgroundLoops(ctx)

	errCh := make(chan error, 1)
	go func() {
		log.Printf("FocusGate listening on http://%s (mode=%s, classifier=%s)",
			cfg.Server.Listen, engine.Mode(), cfg.Classifier.Mode)
		if serveErr := httpServer.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- serveErr
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		return err
	}
}

func runCheck(args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	cfgFlag := fs.String("config", "", "Path to focusgate.yaml")
	_ = fs.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	_, st, engine, err := bootstrap(ctx, *cfgFlag)
	if err != nil {
		return err
	}
	defer st.Close()

	added, err := engine.SyncAll(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("Sync complete: %d new items processed (mode=%s)\n", added, engine.Mode())
	return nil
}

func runDigest(args []string) error {
	fs := flag.NewFlagSet("digest", flag.ExitOnError)
	cfgFlag := fs.String("config", "", "Path to focusgate.yaml")
	_ = fs.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, st, engine, err := bootstrap(ctx, *cfgFlag)
	if err != nil {
		return err
	}
	defer st.Close()

	n, err := engine.FlushDigest(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("Digest dispatched: %d items\n", n)
	return nil
}

func runMode(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: focusgate mode <normal|deep_work|dnd> [--config path]")
	}
	mode := config.FocusMode(args[0])
	fs := flag.NewFlagSet("mode", flag.ExitOnError)
	cfgFlag := fs.String("config", "", "Path to focusgate.yaml")
	_ = fs.Parse(args[1:])

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, st, engine, err := bootstrap(ctx, *cfgFlag)
	if err != nil {
		return err
	}
	defer st.Close()

	if err := engine.SetMode(ctx, mode); err != nil {
		return err
	}
	fmt.Printf("Focus mode set to %s\n", mode)
	return nil
}

func runSimulate(args []string) error {
	fs := flag.NewFlagSet("simulate", flag.ExitOnError)
	cfgFlag := fs.String("config", "", "Path to focusgate.yaml")
	modeFlag := fs.String("mode", "", "Focus mode override (normal|deep_work|dnd)")
	sourceType := fs.String("source", "webhook", "Source type (rss|github|imap|webhook)")
	sourceName := fs.String("source-name", "simulator", "Source name")
	sender := fs.String("sender", "", "Sender")
	title := fs.String("title", "", "Notification title")
	body := fs.String("body", "", "Notification body")
	reason := fs.String("reason", "", "Reason (e.g., review_requested)")
	_ = fs.Parse(args)

	if *title == "" && *body == "" {
		return errors.New("--title or --body is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, st, engine, err := bootstrap(ctx, *cfgFlag)
	if err != nil {
		return err
	}
	defer st.Close()

	dec := engine.SimulateItem(ctx, config.FocusMode(*modeFlag), ingest.IncomingItem{
		SourceType: *sourceType,
		SourceName: *sourceName,
		Sender:     *sender,
		Title:      *title,
		Body:       *body,
		Reason:     *reason,
	})
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(dec)
}

func runVersion() {
	fmt.Println(version.Get().String())
}

func runUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	checkOnly := fs.Bool("check", false, "Only check for new releases without applying")
	repoFlag := fs.String("repo", "", "Custom GitHub repository (defaults to harshitthek/focusgate)")
	_ = fs.Parse(args)

	client := update.NewClient(*repoFlag)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	fmt.Printf("Checking for updates (current: %s)...\n", version.Version)
	res, err := client.Check(ctx)
	if err != nil {
		return fmt.Errorf("checking update: %w", err)
	}

	if !res.HasUpdate {
		fmt.Printf("FocusGate is up to date (%s).\n", res.CurrentVersion)
		return nil
	}

	fmt.Printf("New release available: %s (published %s)\n", res.LatestVersion, res.PublishedAt)
	if res.ReleaseNotes != "" {
		fmt.Printf("\nRelease notes:\n%s\n\n", res.ReleaseNotes)
	}

	if *checkOnly {
		fmt.Printf("Download URL: %s\n", res.DownloadURL)
		return nil
	}

	if res.DownloadURL == "" {
		return errors.New("no compatible release binary found for current operating system and architecture")
	}

	fmt.Printf("Downloading and applying update from %s...\n", res.DownloadURL)
	if err := client.Apply(ctx, res); err != nil {
		return fmt.Errorf("applying update: %w", err)
	}

	fmt.Printf("FocusGate successfully updated to %s. Please restart the daemon.\n", res.LatestVersion)
	return nil
}
