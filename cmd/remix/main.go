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

type config struct {
	port              int
	systemsDir        string
	modelsDir         string
	keepDuplicatesFor time.Duration
}

type application struct {
	config        config
	logger        *slog.Logger
	wg            sync.WaitGroup
	systemMap     map[string]SystemInterface
	schemaMap     map[string]*jsonschema.Schema
	storageEngine *storageEngine
	schemaRootMap map[string]*SchemaRoot
}

func main() {

	var cfg config
	flag.IntVar(&cfg.port, "port", 4000, "API port")
	flag.StringVar(&cfg.systemsDir, "systems-dir", "./systems", "Directory for systems configuration")
	flag.StringVar(&cfg.modelsDir, "models-dir", "./models", "Directory for models configuration")
	flag.DurationVar(&cfg.keepDuplicatesFor, "keep-duplicates-for", 1*time.Hour, "Duration to keep duplicate entries")
	displayVersion := flag.Bool("version", false, "Display version and exit")

	flag.Parse()

	if *displayVersion {
		fmt.Printf("Version:\t%s\n", version)
		os.Exit(0)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	expvar.NewString("version").Set(version)
	expvar.Publish("goroutines", expvar.Func(func() any {
		return runtime.NumGoroutine()
	}))
	expvar.Publish("timestamp", expvar.Func(func() any {
		return time.Now().Unix()
	}))

	schemaMap, schemaRootMap, err := createModelMap(cfg)
	if err != nil {
		logger.Error("failed to load model schemas", "error", err)
		os.Exit(1)
	}

	systemInfoMap, err := createSystemInfoMap(cfg)
	if err != nil {
		logger.Error("failed to load system configurations", "error", err)
		os.Exit(1)
	}

	storageEngine, err := newStorageEngine()
	if err != nil {
		logger.Error("failed to create storage engine", "error", err)
		os.Exit(1)
	}

	app := &application{
		config:        cfg,
		logger:        logger,
		systemMap:     make(map[string]SystemInterface),
		schemaMap:     schemaMap,
		storageEngine: storageEngine,
		schemaRootMap: schemaRootMap,
	}

	serveQuitCh := make(chan struct{})

	duplicateChecker := make(map[string][]ExpiringObject)

	for schemaName := range app.schemaMap {
		duplicateChecker[schemaName] = []ExpiringObject{}
	}

	// The server must be running to check that webhooks are valid (needed for Stripe system initialization)
	go func() {
		err = app.serve()
		if err != nil {
			logger.Error(err.Error())
			os.Exit(1)
		}
		close(serveQuitCh)
	}()

	app.setSystemMap(systemInfoMap, duplicateChecker)
	if err != nil {
		logger.Error("failed to create system map", "error", err)
		os.Exit(1)
	}

	<-serveQuitCh
	app.logger.Info("shutting down server")
}

func createSystemInfoMap(cfg config) (map[string]SystemInfo, error) {
	systemInfoMap := make(map[string]SystemInfo)

	err := filepath.Walk(cfg.systemsDir, func(path string, info os.FileInfo, err error) error {
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

func (app *application) setSystemMap(systemInfoMap map[string]SystemInfo, duplicateChecker map[string][]ExpiringObject) {
	var systemMapMu sync.Mutex
	errCh := make(chan error, len(systemInfoMap))
	doneCh := make(chan struct{}, len(systemInfoMap))

	for systemName, systemInfo := range systemInfoMap {
		go func(systemName string, systemInfo SystemInfo) {
			// Create a new copy of duplicateChecker for each system
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

type Location struct {
	PullObject string `json:"pull_object,omitempty"`
	PushObject string `json:"push_object,omitempty"`
	Field      string `json:"field,omitempty"`
	SearchKey  bool   `json:"search_key,omitempty"`
	Pull       bool   `json:"pull,omitempty"`
	Push       bool   `json:"push,omitempty"`
}

type PropertySystemConfig struct {
	RequireForCreate bool       `json:"require_for_create"`
	Receive          []Location `json:"receive"`
	Push             []Location `json:"push"`
	Sync             []Location `json:"sync"`
}

type Property struct {
	Type    any                             `json:"type"`
	Systems map[string]PropertySystemConfig `json:"systems"`
}

type SchemaRoot struct {
	Title      string              `json:"title"`
	Properties map[string]Property `json:"properties"`
}

func createModelMap(cfg config) (
	schemaMap map[string]*jsonschema.Schema,
	schemaRoot map[string]*SchemaRoot,
	err error,
) {

	jsonFiles := []string{}

	err = filepath.WalkDir(cfg.modelsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Skip directories and non-JSON files
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}
		jsonFiles = append(jsonFiles, path)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	schemaMap = make(map[string]*jsonschema.Schema)
	schemaRootMap := map[string]*SchemaRoot{}
	compiler := jsonschema.NewCompiler()

	for _, path := range jsonFiles {

		schemaRoot := &SchemaRoot{}

		url := "file://" + filepath.ToSlash(path)
		schema, err := compiler.Compile(url)
		if err != nil {
			return nil, nil, fmt.Errorf("compile %s: %w", path, err)
		}

		if schema.Title == "" {
			return nil, nil, fmt.Errorf("schema %s has no title", path)
		}

		schemaMap[schema.Title] = schema

		f, err := os.Open(path)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to open model file %s: %w", path, err)
		}
		defer f.Close()

		decoder := json.NewDecoder(f)
		if err := decoder.Decode(&schemaRoot); err != nil {
			return nil, nil, fmt.Errorf("failed to decode model file %s: %w", path, err)
		}

		schemaRootMap[schema.Title] = schemaRoot
	}

	return schemaMap, schemaRootMap, nil
}
