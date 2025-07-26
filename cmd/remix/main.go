package main

import (
	"encoding/json"
	"expvar"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/sqlpipe/remix/internal/app"
	"github.com/sqlpipe/remix/internal/systems"
	"github.com/sqlpipe/remix/internal/vcs"
	"gopkg.in/yaml.v3"
)

func main() {
	// Parse command-line flags into app.Config
	flag.IntVar(&app.Config.Port, "port", 4000, "API port")
	flag.StringVar(&app.Config.ConfigDir, "config-dir", "./config", "Directory for config files")
	flag.IntVar(&app.Config.DuplicateCacheSize, "duplicate-cache-size", 256, "Size of the duplicate cache in MB")
	flag.StringVar(&app.Config.LogLevel, "log-level", "info", "Log level: debug, info, warn, error")
	flag.BoolVar(&app.Config.DisplayVersion, "version", false, "Display version and exit")

	flag.Parse()

	if app.Config.DisplayVersion {
		// Print version and exit if --version flag is set
		fmt.Printf("Version:\t%s\n", vcs.Version())
		os.Exit(0)
	}

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

	var err error
	var systemInfoMap map[string]systems.SystemInfo

	// Load model schemas and system configs from config directory
	err = setMaps()
	if err != nil {
		app.Logger.Error("failed to set maps", "error", err)
		os.Exit(1)
	}

	b, _ := json.MarshalIndent(app.SchemaMap, "", "  ")
	app.Logger.Debug("Loaded schema map", "schemaMap", string(b))

	b, _ = json.MarshalIndent(systemInfoMap, "", "  ")
	app.Logger.Debug("Loaded system info map", "systemInfoMap", string(b))

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

// setMaps loads all schema and system config files from the config directory and populates the global maps.
func setMaps() error {
	schemaFiles, systemFiles, err := findConfigFiles()
	if err != nil {
		return fmt.Errorf("failed to find config files: %w", err)
	}

	err = setSchemaMap(schemaFiles)
	if err != nil {
		return fmt.Errorf("failed to compile schemas: %w", err)
	}

	err = setSystemMap(systemFiles)
	if err != nil {
		return fmt.Errorf("failed to set system map: %w", err)
	}

	return nil
}

// findConfigFiles walks the config directory and returns lists of schema (.json) and system (.yaml/.yml) files.
func findConfigFiles() (schemaFiles []string, systemFiles []string, err error) {
	err = filepath.WalkDir(app.Config.ConfigDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Only process files
		if d.IsDir() {
			return nil
		}
		// Add .json files to schemaFiles
		if strings.HasSuffix(d.Name(), ".json") {
			app.Logger.Debug("found schema file", "path", path)
			schemaFiles = append(schemaFiles, path)
			return nil
		}
		// Add .yaml or .yml files to systemFiles
		if strings.HasSuffix(d.Name(), ".yaml") || strings.HasSuffix(d.Name(), ".yml") {
			app.Logger.Debug("found system file", "path", path)
			systemFiles = append(systemFiles, path)
			return nil
		}
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to walk config dir: %w", err)
	}

	return schemaFiles, systemFiles, nil
}

// setSchemaMap compiles all JSON schema files and populates app.SchemaMap.
func setSchemaMap(schemaFiles []string) error {
	schemaMap := map[string]*jsonschema.Schema{}
	compiler := jsonschema.NewCompiler()

	for _, path := range schemaFiles {
		url := "file://" + filepath.ToSlash(path)
		schema, err := compiler.Compile(url)
		if err != nil {
			return fmt.Errorf("failed to compile schema at path %s: %w", path, err)
		}

		if schema.Title == "" {
			return fmt.Errorf("schema at path %s has no title", path)
		}

		schemaMap[schema.Title] = schema
	}

	b, _ := json.MarshalIndent(schemaMap, "", "  ")
	app.Logger.Debug("Compiled schema map", "schemaMap", string(b))

	app.SchemaMap = schemaMap

	return nil
}

// setSystemMap loads all system YAML files, decodes them into SystemInfo, and initializes systems in the global map.
func setSystemMap(systemFiles []string) error {
	systemInfoMap := map[string]systems.SystemInfo{}

	for _, path := range systemFiles {
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("failed to open system file at path %s: %w", path, err)
		}
		defer f.Close()

		var sysInfo systems.SystemInfo
		decoder := yaml.NewDecoder(f)
		err = decoder.Decode(&sysInfo)
		if err != nil {
			return fmt.Errorf("failed to decode system file %s: %w", path, err)
		}
		if sysInfo.Name == "" {
			return fmt.Errorf("system file at path %s has no name", path)
		}
		systemInfoMap[sysInfo.Name] = sysInfo
	}

	for _, systemInfo := range systemInfoMap {
		// Initialize each system and store in the global map
		system, err := systems.NewSystem(systemInfo)
		if err != nil {
			app.Logger.Error("failed to initialize system", "error", err)
			os.Exit(1)
		}

		app.ObjectStore.SetSafeIndexMap(systemInfo.Name, 0)

		systems.SystemMap[systemInfo.Name] = system
	}

	return nil
}
