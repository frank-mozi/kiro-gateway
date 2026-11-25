// Command kiro-gateway is the Kiro Gateway server entrypoint. It loads configuration,
// builds the application, and serves until interrupted.
//
// Usage:
//
//	kiro-gateway [command] [flags]
//
// Commands:
//
//	portable     run the portable gateway (default when no command is given)
//	start        start the server with ordinary configuration
//	status       check whether a local server is running and print its URL
//	bootstrap    create an initial API key and exit
//	version      print version and exit
//	help         show usage
//
// For backward compatibility the legacy flag form is still accepted, so
// `kiro-gateway -bootstrap`, `kiro-gateway -healthcheck`, and
// `kiro-gateway -version` keep working.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/mydisha/keirouter/backend/internal/app"
	"github.com/mydisha/keirouter/backend/internal/config"
	"github.com/mydisha/keirouter/backend/internal/portable"
	"github.com/mydisha/keirouter/backend/internal/prettylog"
	"github.com/mydisha/keirouter/backend/internal/tray"
	"github.com/mydisha/keirouter/backend/internal/version"
)

// Version is set at build time via -ldflags "-X main.Version=...".
// Defaults to "dev" for local builds. When ldflags are not injected (e.g. a
// Docker/PaaS build with no git), version.Resolve falls back to the committed
// VERSION file so the dashboard still reports a real version.
var Version = "dev"

func init() {
	runtime.LockOSThread()
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		if !errors.Is(err, errSilent) {
			fmt.Fprintln(os.Stderr, "kiro-gateway:", err)
		}
		os.Exit(1)
	}
}

// errSilent signals a non-zero exit without printing the default error prefix.
// Used by commands that already printed a user-friendly message.
var errSilent = errors.New("silent")

// No arguments select portable mode; explicit commands and legacy flags retain
// their existing behavior.
func run(args []string) error {
	if len(args) == 0 {
		return cmdPortable(nil)
	}
	if len(args) > 0 {
		switch args[0] {
		case "help", "-h", "--help":
			printUsage(os.Stdout)
			return nil
		}
	}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd := args[0]
		rest := args[1:]
		switch cmd {
		case "portable":
			return cmdPortable(rest)
		case "release":
			return cmdRelease(rest)
		case "start":
			return cmdStart(rest)
		case "tray":
			return cmdTray(rest)
		case "status":
			return cmdStatus(rest)
		case "bootstrap":
			return cmdBootstrap(rest)
		case "version":
			fmt.Println("Kiro Gateway", version.Resolve(Version))
			return nil
		default:
			printUsage(os.Stderr)
			return fmt.Errorf("unknown command %q", cmd)
		}
	}
	// Legacy flag form (no subcommand). Equivalent to `start`, but also honors
	// -bootstrap, -healthcheck, and -version for backward compatibility.
	return runLegacy(args)
}

func printUsage(w *os.File) {
	fmt.Fprint(w, `Kiro Gateway — self-hostable AI gateway

Usage:
  kiro-gateway [command] [flags]

Commands:
  portable     Run a portable gateway (default with no arguments; no service install)
  release      Create signing keys or publish a local release (see release --help)
  start        Start the server with ordinary configuration and password login
  tray         Start the server in background with a system tray icon
  status       Check whether a local server is running and print its URL
  bootstrap    Create an initial API key and print it once
  version      Print version and exit
  help         Show this help

Common flags:
  -config <path>     Path to a YAML config file (optional)
  --tray             (start) Run with system tray icon in menu bar / taskbar
  --no-browser       (portable, start, tray) Do not open the dashboard in a browser
  -key-name <name>   (bootstrap) Name for the created API key

Examples:
  kiro-gateway                        # portable mode; Windows also supports double-click
  kiro-gateway portable --port 20311  # portable mode with a custom loopback port
  kiro-gateway start                  # start the server, open the dashboard
  kiro-gateway start --tray           # start in background with system tray icon
  kiro-gateway tray                   # shorthand for start --tray
  kiro-gateway start --no-browser     # start without opening a browser
  kiro-gateway bootstrap              # mint your first API key
  kiro-gateway status                 # is it running?
`)
}

// cmdStart parses start-specific flags and serves.
func cmdStart(args []string) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to a YAML config file (optional)")
	noBrowser := fs.Bool("no-browser", false, "do not open the dashboard in a browser")
	useTray := fs.Bool("tray", false, "run with system tray icon in menu bar / taskbar")
	managed := fs.Bool("managed", false, "internal: stop when the portable launcher closes stdin")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, log, isPretty, err := setup(*configPath)
	if err != nil {
		return err
	}
	if *managed {
		client, err := portable.ClientFromEnvironment()
		if err != nil {
			return err
		}
		if client == nil || *useTray {
			return errors.New("managed start requires the portable launcher and does not support tray mode")
		}
		cfg.Database.Driver, cfg.Database.DSN = "sqlite", ""
		cfg.Security.MasterKey = ""
		cfg.Server.Host = "127.0.0.1"
		cfg.Security.BindLoopbackOnly = true
		return serveManaged(cfg, log)
	}
	// Open the dashboard by default, but only on an interactive terminal so
	// headless/Docker runs stay quiet. --no-browser always wins.
	openBrowser := isPretty && !*noBrowser
	if *useTray {
		return serveWithTray(cfg, log, isPretty, openBrowser)
	}

	return serve(cfg, log, isPretty, openBrowser)
}

func serveManaged(cfg config.Config, log *slog.Logger) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go func() {
		if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
			log.Warn("portable control input closed", "err", err)
		}
		cancel()
	}()
	application, err := app.Build(ctx, cfg, log, version.Resolve(Version))
	if err != nil {
		return err
	}
	return application.Run(ctx)
}

// cmdTray starts the server with system tray enabled.
func cmdTray(args []string) error {
	fs := flag.NewFlagSet("tray", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to a YAML config file (optional)")
	noBrowser := fs.Bool("no-browser", false, "do not open the dashboard in a browser")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, log, isPretty, err := setup(*configPath)
	if err != nil {
		return err
	}
	openBrowser := isPretty && !*noBrowser
	return serveWithTray(cfg, log, isPretty, openBrowser)
}

// cmdBootstrap creates an initial API key and exits.
func cmdBootstrap(args []string) error {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to a YAML config file (optional)")
	keyName := fs.String("key-name", "default", "name for the bootstrapped API key")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, _, _, err := setup(*configPath)
	if err != nil {
		return err
	}
	return doBootstrap(cfg, *keyName)
}

// cmdStatus reports whether a local server is reachable.
func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to a YAML config file (optional)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	url := dashboardURL(cfg)
	if err := runHealthcheck(cfg); err != nil {
		fmt.Printf("Kiro Gateway is not running (no response at %s)\n", url)
		return errSilent
	}
	fmt.Printf("Kiro Gateway is running → %s\n", url)
	return nil
}

// runLegacy preserves the original flag-based entrypoint so existing usage
// (`keirouter -bootstrap`, `keirouter -healthcheck`,
// `keirouter -version`) keeps working unchanged.
func runLegacy(args []string) error {
	fs := flag.NewFlagSet("kiro-gateway", flag.ContinueOnError)
	showVersion := fs.Bool("version", false, "print version and exit")
	configPath := fs.String("config", "", "path to a YAML config file (optional)")
	bootstrap := fs.Bool("bootstrap", false, "create an initial API key and exit")
	healthcheck := fs.Bool("healthcheck", false, "check the local HTTP health endpoint and exit")
	keyName := fs.String("key-name", "default", "name for the bootstrapped API key")
	useTray := fs.Bool("tray", false, "run with system tray icon in menu bar / taskbar")
	noBrowser := fs.Bool("no-browser", false, "do not open the dashboard in a browser")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *showVersion {
		fmt.Println("Kiro Gateway", version.Resolve(Version))
		return nil
	}

	cfg, log, isPretty, err := setup(*configPath)
	if err != nil {
		return err
	}

	if *healthcheck {
		return runHealthcheck(cfg)
	}
	if *bootstrap {
		return doBootstrap(cfg, *keyName)
	}
	openBrowser := isPretty && !*noBrowser
	if *useTray {
		return serveWithTray(cfg, log, isPretty, openBrowser)
	}
	// Legacy flag-only server invocations do not auto-open a browser.
	return serve(cfg, log, isPretty, false)
}

// setup loads configuration and initializes the default logger.
func setup(configPath string) (config.Config, *slog.Logger, bool, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return config.Config{}, nil, false, fmt.Errorf("load config: %w", err)
	}
	log, isPretty := newLogger(cfg.Log)
	slog.SetDefault(log)
	return cfg, log, isPretty, nil
}

// serve builds the application and runs it until interrupted. When autoOpenBrowser
// is true it launches the dashboard once the server reports healthy.
func serve(cfg config.Config, log *slog.Logger, isPretty, autoOpenBrowser bool) error {
	resolvedVersion := version.Resolve(Version)

	// Print the startup banner in pretty mode (TTY only).
	if isPretty {
		cacheLabel := "disabled"
		if cfg.Cache.Enabled {
			cacheLabel = cfg.Cache.Backend
		}
		dataDir := cfg.Data.Dir
		if dataDir == "" {
			dataDir = "~/.keirouter"
		}
		prettylog.PrintBannerStdout(prettylog.BannerConfig{
			Version:  resolvedVersion,
			Addr:     cfg.Addr(),
			DBDriver: cfg.Database.Driver,
			Cache:    cacheLabel,
			DataDir:  dataDir,
			LogLevel: cfg.Log.Level + " (pretty)",
		})
	}

	// Cancel on SIGINT/SIGTERM for graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if autoOpenBrowser {
		go openDashboardWhenReady(ctx, cfg)
	}

	application, err := app.Build(ctx, cfg, log, resolvedVersion)
	if err != nil {
		return fmt.Errorf("build app: %w", err)
	}

	return application.Run(ctx)
}

// serveWithTray builds the application and runs it while pumping the system
// tray message loop on the main OS thread.
func serveWithTray(cfg config.Config, log *slog.Logger, isPretty, autoOpenBrowser bool) error {
	resolvedVersion := version.Resolve(Version)

	if isPretty {
		cacheLabel := "disabled"
		if cfg.Cache.Enabled {
			cacheLabel = cfg.Cache.Backend
		}
		dataDir := cfg.Data.Dir
		if dataDir == "" {
			dataDir = "~/.keirouter"
		}
		prettylog.PrintBannerStdout(prettylog.BannerConfig{
			Version:  resolvedVersion,
			Addr:     cfg.Addr(),
			DBDriver: cfg.Database.Driver,
			Cache:    cacheLabel,
			DataDir:  dataDir,
			LogLevel: cfg.Log.Level + " (pretty)",
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	if autoOpenBrowser {
		go openDashboardWhenReady(ctx, cfg)
	}

	application, err := app.Build(ctx, cfg, log, resolvedVersion)
	if err != nil {
		return fmt.Errorf("build app: %w", err)
	}

	dashURL := dashboardURL(cfg)
	t := tray.New(tray.Options{
		Version:      resolvedVersion,
		DashboardURL: dashURL,
		Port:         cfg.Server.Port,
		OnOpenURL:    openBrowser,
		OnQuit: func() {
			cancel()
		},
		Logger: log,
	})

	go func() {
		select {
		case sig := <-sigCh:
			log.Info("received termination signal", "signal", sig)
			cancel()
		case <-ctx.Done():
		}
	}()

	return runTrayLifecycle(ctx, cancel, application.Run, t)
}

type trayLifecycle interface {
	Run(context.Context) error
	Stop()
}

// runTrayLifecycle keeps the HTTP server and tray event loop in the same
// lifecycle. The tray must run on the calling OS thread, while the server runs
// in the background. Whichever side exits first cancels and stops the other.
func runTrayLifecycle(
	ctx context.Context,
	cancel context.CancelFunc,
	runServer func(context.Context) error,
	t trayLifecycle,
) error {
	serverErrCh := make(chan error, 1)
	go func() {
		serverErrCh <- runServer(ctx)
		cancel()
		t.Stop()
	}()

	// Keep the native tray message loop on the main OS thread.
	trayErr := t.Run(ctx)
	cancel()
	t.Stop()
	serverErr := <-serverErrCh

	if trayErr != nil {
		return fmt.Errorf("system tray: %w", trayErr)
	}
	return serverErr
}

// doBootstrap creates an initial API key and prints it once.
func doBootstrap(cfg config.Config, keyName string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	plaintext, err := app.Bootstrap(ctx, cfg, keyName)
	if err != nil {
		return err
	}
	fmt.Println("Created API key (copy it now, it will not be shown again):")
	fmt.Println(plaintext)
	return nil
}

func runHealthcheck(cfg config.Config) error {
	resp, err := http.Get(healthURL(cfg))
	if err != nil {
		return fmt.Errorf("healthcheck %s: %w", healthURL(cfg), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("healthcheck %s: %s", healthURL(cfg), resp.Status)
	}
	return nil
}

// loopbackHost normalizes a wildcard/empty bind address to a reachable
// loopback host for local client connections.
func loopbackHost(cfg config.Config) string {
	host := cfg.Server.Host
	if host == "" || host == "0.0.0.0" || host == "::" {
		return "127.0.0.1"
	}
	return host
}

func healthURL(cfg config.Config) string {
	return fmt.Sprintf("http://%s:%d/healthz", loopbackHost(cfg), cfg.Server.Port)
}

// dashboardURL is the friendly URL to show users and open in a browser.
func dashboardURL(cfg config.Config) string {
	host := loopbackHost(cfg)
	if host == "127.0.0.1" {
		host = "localhost"
	}
	return fmt.Sprintf("http://%s:%d", host, cfg.Server.Port)
}

// openDashboardWhenReady polls the health endpoint until the server is up (or
// ctx is cancelled), then opens the dashboard in the default browser. Failures
// are best-effort: the user can always open the URL printed in the banner.
func openDashboardWhenReady(ctx context.Context, cfg config.Config) {
	client := &http.Client{Timeout: 1 * time.Second}
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(30 * time.Second)

	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			return
		case <-ticker.C:
			resp, err := client.Get(healthURL(cfg))
			if err != nil {
				continue
			}
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				openBrowser(dashboardURL(cfg))
				return
			}
		}
	}
}

// openBrowser opens url in the system default browser. Best-effort; errors are
// ignored because the URL is also printed to the console.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		slog.Warn("cannot open dashboard browser; use the printed URL", "err", err)
	}
}

// newLogger builds a structured logger per config. Returns the logger and
// whether pretty (colorized) output is active.
func newLogger(cfg config.LogConfig) (*slog.Logger, bool) {
	var level slog.Level
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	pretty := false
	if cfg.Format == "json" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = prettylog.NewHandler(os.Stdout, &prettylog.Options{Level: level})
		pretty = prettylog.IsTerminal(os.Stdout.Fd())
	}
	return slog.New(handler), pretty
}
