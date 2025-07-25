package main

import (
	"encoding/json"
	"expvar"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/sqlpipe/remix/internal/vcs"

	"gopkg.in/yaml.v3"

	_ "github.com/jackc/pgx/v5/stdlib"
)

var (
	version = vcs.Version()
)

// Main entry point for the Remix application. Handles configuration, initialization, and server startup.
type config struct {
	port              int
	configDir         string
	keepDuplicatesFor time.Duration
	logLevel          string
}

// application is the main struct holding global state and dependencies for the server.
type application struct {
	config        config
	logger        *slog.Logger
	wg            sync.WaitGroup
	storageEngine *storageEngine
	schemaMap     map[string]*SchemaRoot
	systemMap     map[string]SystemInterface
}

// main parses flags, initializes logging, loads schemas and systems, and starts the server.
func main() {

	var cfg config
	flag.IntVar(&cfg.port, "port", 4000, "API port")
	flag.StringVar(&cfg.configDir, "config-dir", "./config", "Directory for config files")
	flag.DurationVar(&cfg.keepDuplicatesFor, "keep-duplicates-for", 1*time.Hour, "Duration to keep duplicate entries")
	flag.StringVar(&cfg.logLevel, "log-level", "info", "Log level: debug, info, warn, error")
	displayVersion := flag.Bool("version", false, "Display version and exit")

	flag.Parse()

	if *displayVersion {
		fmt.Printf("Version:\t%s\n", version)
		os.Exit(0)
	}

	// Map string flag to slog.Level
	var level slog.Level
	switch strings.ToLower(cfg.logLevel) {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		fmt.Fprintf(os.Stderr, "Unknown log level: %s. Using info.\n", cfg.logLevel)
		level = slog.LevelInfo
	}

	// Set up slog logger with dynamic level based on log-level flag
	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	logger := slog.New(handler)

	// Register expvar metrics for monitoring.
	expvar.NewString("version").Set(version)
	expvar.Publish("goroutines", expvar.Func(func() any {
		return runtime.NumGoroutine()
	}))
	expvar.Publish("timestamp", expvar.Func(func() any {
		return time.Now().Unix()
	}))

	// Load model schemas from config directory.
	schemaMap, systemInfoMap, err := createMaps(cfg, logger)
	if err != nil {
		logger.Error("failed to create schema map", "error", err)
		os.Exit(1)
	}
	b, _ := json.MarshalIndent(schemaMap, "", "  ")
	logger.Debug("Loaded schema map", "schemaMap", string(b))

	b, _ = json.MarshalIndent(systemInfoMap, "", "  ")
	logger.Debug("Loaded system info map", "systemInfoMap", string(b))

	// Initialize storage engine (e.g., database or in-memory store).
	storageEngine, err := newStorageEngine()
	if err != nil {
		logger.Error("failed to create storage engine", "error", err)
		os.Exit(1)
	}

	// Construct the main application struct.
	app := &application{
		config:        cfg,
		logger:        logger,
		storageEngine: storageEngine,
		systemMap:     make(map[string]SystemInterface),
		schemaMap:     schemaMap,
	}

	serveQuitCh := make(chan struct{})

	// duplicateChecker tracks recently seen objects to prevent duplicate processing.
	duplicateChecker := make(map[string][]ExpiringObject)

	for schemaName := range app.schemaMap {
		duplicateChecker[schemaName] = []ExpiringObject{}
	}

	// Start the HTTP server in a goroutine so that webhooks can be validated during system initialization.
	go func() {
		err = app.serve()
		if err != nil {
			logger.Error(err.Error())
			os.Exit(1)
		}
		close(serveQuitCh)
	}()

	// Initialize all systems in parallel and populate app.systemMap.
	app.setSystemMap(systemInfoMap, duplicateChecker)
	if err != nil {
		logger.Error("failed to create system map", "error", err)
		os.Exit(1)
	}

	// Wait for server shutdown signal.
	<-serveQuitCh
	app.logger.Info("shutting down server")
}

// createSystemInfoMap loads all system YAML files from the config directory and returns a map of system names to SystemInfo structs.
func createSystemInfoMap(cfg config, logger *slog.Logger) (map[string]SystemInfo, error) {
	systemInfoMap := make(map[string]SystemInfo)

	err := filepath.Walk(cfg.configDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return fmt.Errorf("error walking path %s: %w", path, err)
		}

		if !info.IsDir() && (filepath.Ext(path) == ".yaml" || filepath.Ext(path) == ".yml") {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()

			var infos map[string]SystemInfo
			decoder := yaml.NewDecoder(f)
			err = decoder.Decode(&infos)
			if err != nil {
				if err == io.EOF {
					// Empty YAML file, skip it
					return nil
				}
				return err
			}
			for name, info := range infos {
				info.Name = name
				systemInfoMap[info.Name] = info
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to walk systems dir: %w", err)
	}

	return systemInfoMap, nil
}

// setSystemMap initializes all systems concurrently and populates the application's systemMap.
// It also handles error collection and ensures thread safety.
func (app *application) setSystemMap(systemInfoMap map[string]SystemInfo, duplicateChecker map[string][]ExpiringObject) {
	var systemMapMu sync.Mutex
	errCh := make(chan error, len(systemInfoMap))
	doneCh := make(chan struct{}, len(systemInfoMap))

	for systemName, systemInfo := range systemInfoMap {
		go func(systemName string, systemInfo SystemInfo) {
			// Create a new copy of duplicateChecker for each system to avoid data races.
			dupCheckerCopy := make(map[string][]ExpiringObject, len(duplicateChecker))
			for k, v := range duplicateChecker {
				// Create a new slice for each key to avoid sharing underlying arrays
				copiedSlice := make([]ExpiringObject, len(v))
				copy(copiedSlice, v)
				dupCheckerCopy[k] = copiedSlice
			}

			system, err := app.NewSystem(systemInfo, app.config.port, dupCheckerCopy)
			if err != nil {
				errCh <- err
			} else {
				systemMapMu.Lock()
				app.systemMap[systemInfo.Name] = system
				systemMapMu.Unlock()
			}
			doneCh <- struct{}{}
		}(systemName, systemInfo)
	}

	// Wait for all goroutines to finish
	for i := 0; i < len(systemInfoMap); i++ {
		<-doneCh
	}

	// Collect and handle errors from errCh
	var systemInitErrs []error
	for e := range errCh {
		systemInitErrs = append(systemInitErrs, e)
	}
	if len(systemInitErrs) > 0 {
		app.logger.Error("failed to initialize one or more systems", "errors", systemInitErrs)
		os.Exit(1)
	}
}

// Location describes where a property is found or pushed in a system.
type Location struct {
	PullObject string `json:"pull_object,omitempty"`
	PushObject string `json:"push_object,omitempty"`
	Field      string `json:"field,omitempty"`
	SearchKey  bool   `json:"search_key,omitempty"`
	Pull       bool   `json:"pull,omitempty"`
	Push       bool   `json:"push,omitempty"`
}

// PropertySystemConfig defines how a property is handled for a specific system.
type PropertySystemConfig struct {
	// RequireForCreate bool       `json:"require_for_create"`
	Receive []Location `json:"receive"`
	Push    []Location `json:"push"`
	// Sync    []Location `json:"sync"`
}

// Property represents a schema property and its system-specific configuration.
type Property struct {
	Type    []string                        `json:"type"`
	Systems map[string]PropertySystemConfig `json:"systems"`
}

// SchemaRoot is the root of a model schema, including its title and properties.
type SchemaRoot struct {
	Title      string              `json:"title"`
	Validator  *jsonschema.Schema  `json:"-"`
	Properties map[string]Property `json:"properties"`
}

// createSchemaMap loads all JSON schema files from the config directory, compiles them, and returns both the compiled schemas and their root structures.
func createMaps(cfg config, logger *slog.Logger) (
	schemaMap map[string]*SchemaRoot,
	systemInfoMap map[string]SystemInfo,
	err error,
) {

	schemaFiles := []string{}
	systemFiles := []string{}

	err = filepath.WalkDir(cfg.configDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Skip directories
		if d.IsDir() {
			return nil
		}
		// Add .json files to schemaFiles
		if strings.HasSuffix(d.Name(), ".json") {
			logger.Debug("found schema file", "path", path)
			schemaFiles = append(schemaFiles, path)
			return nil
		}
		// Add .yaml or .yml files to systemFiles
		if strings.HasSuffix(d.Name(), ".yaml") || strings.HasSuffix(d.Name(), ".yml") {
			logger.Debug("found system file", "path", path)
			systemFiles = append(systemFiles, path)
			return nil
		}
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to walk config dir: %w", err)
	}

	schemaMap = map[string]*SchemaRoot{}
	compiler := jsonschema.NewCompiler()

	for _, path := range schemaFiles {

		schemaRoot := &SchemaRoot{}

		url := "file://" + filepath.ToSlash(path)
		schema, err := compiler.Compile(url)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to compile schema at path %s: %w", path, err)
		}

		if schema.Title == "" {
			return nil, nil, fmt.Errorf("schema at path %s has no title", path)
		}

		schemaRoot.Validator = schema

		f, err := os.Open(path)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to open schema file at path %s: %w", path, err)
		}
		defer f.Close()

		decoder := json.NewDecoder(f)
		if err := decoder.Decode(&schemaRoot); err != nil {
			return nil, nil, fmt.Errorf("failed to decode model file %s: %w", path, err)
		}

		schemaMap[schema.Title] = schemaRoot
	}

	numSchemas := len(schemaMap)
	logger.Debug("Number of schemas", "numSchemas", numSchemas)

	systemInfoMap = map[string]SystemInfo{}

	// Load all system YAML files from the config directory and populate systemInfoMap.
	for _, path := range systemFiles {
		f, err := os.Open(path)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to open system file at path %s: %w", path, err)
		}
		defer f.Close()

		var sysInfo SystemInfo
		decoder := yaml.NewDecoder(f)
		err = decoder.Decode(&sysInfo)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to decode system file %s: %w", path, err)
		}
		if sysInfo.Name == "" {
			return nil, nil, fmt.Errorf("system file at path %s has no name", path)
		}
		systemInfoMap[sysInfo.Name] = sysInfo
	}

	numSystems := len(systemInfoMap)
	logger.Debug("Number of systems", "numSystems", numSystems)

	return schemaMap, systemInfoMap, nil
}
