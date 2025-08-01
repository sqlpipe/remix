package main

import (
	"expvar"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/sqlpipe/remix/internal/app"
	"github.com/sqlpipe/remix/internal/vcs"
)

func main() {
	// Parse command-line flags into app.Config
	parseFlags()

	// Print version and exit if --version flag is set
	if app.Config.DisplayVersion {
		fmt.Printf("Version:\t%s\n", vcs.Version())
		os.Exit(0)
	}

	// Map string log level to slog.Level
	setLogLevel()

	// Register expvar metrics for monitoring and introspection
	registerExpvarMetrics()

	// Load model schemas and system configs from config directory
	err := setMaps()
	if err != nil {
		app.Logger.Error("failed to set maps", "error", err)
		os.Exit(1)
	}

	// Set up and start the HTTP server
	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", app.Config.Port),
		Handler:      routes(),
		IdleTimeout:  time.Minute,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	app.Logger.Info("Starting remix API server", "port", app.Config.Port)
	err = srv.ListenAndServe()
	if err != nil {
		app.Logger.Error("failed to start server", "error", err)
		os.Exit(1)
	}
}

func registerExpvarMetrics() {
	expvar.NewString("version").Set(vcs.Version())
	expvar.Publish("goroutines", expvar.Func(func() any {
		return runtime.NumGoroutine()
	}))
	expvar.Publish("timestamp", expvar.Func(func() any {
		return time.Now().Unix()
	}))
}

func parseFlags() {
	flag.IntVar(&app.Config.Port, "port", 4000, "API port")
	flag.StringVar(&app.Config.ConfigDir, "config-dir", "./config", "Directory for config files")
	flag.IntVar(&app.Config.DuplicateCacheSize, "duplicate-cache-size", 256, "Size of the duplicate cache in MB")
	flag.StringVar(&app.Config.LogLevel, "log-level", "info", "Log level: debug, info, warn, error")
	flag.BoolVar(&app.Config.DisplayVersion, "version", false, "Display version and exit")

	flag.BoolVar(&app.Config.Limiter.Enabled, "limiter-enabled", true, "Enable rate limiter")
	flag.Float64Var(&app.Config.Limiter.Rps, "limiter-rps", 25, "Rate limiter maximum requests per second")
	flag.IntVar(&app.Config.Limiter.Burst, "limiter-burst", 100, "Rate limiter maximum burst")

	flag.IntVar(&app.Config.MaxSnapshots, "max-snapshots", 100, "Maximum number of object store snapshots to keep in debug mode")
	flag.IntVar(&app.Config.MaxRAMSizeMB, "max-ram-mb", 1024, "Maximum RAM size for seen objects in MB")

	flag.Parse()
}

func setLogLevel() {
	var level slog.Level
	switch strings.ToLower(app.Config.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		fmt.Fprintf(os.Stderr, "Unknown log level: %s. Using info.\n", app.Config.LogLevel)
		level = slog.LevelInfo
	}

	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	app.Logger = slog.New(handler)
}
