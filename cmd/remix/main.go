package main

import (
	"encoding/json"
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
	"github.com/sqlpipe/remix/internal/systems"
	"github.com/sqlpipe/remix/internal/vcs"
)

func main() {
	// Parse command-line flags into app.Config
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

	if app.Config.DisplayVersion {
		// Print version and exit if --version flag is set
		fmt.Printf("Version:\t%s\n", vcs.Version())
		os.Exit(0)
	}

	// Set the MaxRAMSize for seen objects from the config value (in MB)
	// FirstObject    *Object
	// CurrentRAMSize int64
	// MaxRAMSize     int64
	// app.DuplicateChecker.MaxRAMSize = int64(app.Config.MaxRAMSizeMB) * 1024 * 1024

	// Map string log level to slog.Level
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

	// Set up slog logger with the chosen log level
	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	app.Logger = slog.New(handler)

	// Register expvar metrics for monitoring and introspection
	expvar.NewString("version").Set(vcs.Version())
	expvar.Publish("goroutines", expvar.Func(func() any {
		return runtime.NumGoroutine()
	}))
	expvar.Publish("timestamp", expvar.Func(func() any {
		return time.Now().Unix()
	}))

	// Load model schemas and system configs from config directory
	err := setMaps()
	if err != nil {
		app.Logger.Error("failed to set maps", "error", err)
		os.Exit(1)
	}

	keys := make([]string, 0, len(app.SchemaMap))
	for k := range app.SchemaMap {
		keys = append(keys, k)
	}
	app.Logger.Debug(fmt.Sprintf("schema map keys: %v", keys))

	b, _ := json.MarshalIndent(systems.SystemMap, "", "  ")
	app.Logger.Debug("Set system map", "systemMap", string(b))

	app.Logger.Info("Starting remix API server", "port", app.Config.Port)

	// Set up and start the HTTP server
	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", app.Config.Port),
		Handler:      routes(),
		IdleTimeout:  time.Minute,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	err = srv.ListenAndServe()
	if err != nil {
		app.Logger.Error("failed to start server", "error", err)
		os.Exit(1)
	}
}
