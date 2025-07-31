package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"

	"encoding/json"

	"github.com/sqlpipe/remix/internal/app"
	"github.com/sqlpipe/remix/internal/systems"
)

// setMaps loads all schema and system config files from the config directory and populates the global maps.
func setMaps() error {
	schemaFiles, systemFiles, err := findConfigFiles()
	if err != nil {
		return fmt.Errorf("failed to find config files: %w", err)
	}

	app.Logger.Debug(fmt.Sprintf("schema files: %v", schemaFiles))
	app.Logger.Debug(fmt.Sprintf("system files: %v", systemFiles))

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
			schemaFiles = append(schemaFiles, path)
			return nil
		}
		// Add .yaml or .yml files to systemFiles
		if strings.HasSuffix(d.Name(), ".yaml") || strings.HasSuffix(d.Name(), ".yml") {
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

// readRawSchema reads and decodes the JSON schema file.
func readRawSchema(path string) (map[string]any, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open schema file %s: %w", path, err)
	}
	defer file.Close()

	raw := map[string]any{}
	if err := json.NewDecoder(file).Decode(raw); err != nil {
		return nil, fmt.Errorf("failed to decode schema file %s: %w", path, err)
	}
	return raw, nil
}

// extractSearchKeys extracts the search_keys field as a []string.
func extractSearchKeys(raw map[string]any) []string {
	var searchKeys []string
	if keys, ok := raw["search_keys"].([]string); ok {
		for _, key := range keys {
			searchKeys = append(searchKeys, key)
		}
	}
	return searchKeys
}

// compileValidator compiles the schema file and returns the validator.
func compileValidator(path string, compiler *jsonschema.Compiler) (*jsonschema.Schema, error) {
	url := "file://" + filepath.ToSlash(path)
	validator, err := compiler.Compile(url)
	if err != nil {
		return nil, fmt.Errorf("failed to compile schema at path %s: %w", path, err)
	}
	if validator.Title == "" {
		return nil, fmt.Errorf("schema at path %s has no title", path)
	}
	return validator, nil
}

// setSchemaMap compiles all JSON schema files and populates app.SchemaMap.
func setSchemaMap(schemaFiles []string) error {
	compiler := jsonschema.NewCompiler()

	for _, path := range schemaFiles {
		raw, err := readRawSchema(path)
		if err != nil {
			return err
		}
		searchKeys := extractSearchKeys(raw)
		validator, err := compileValidator(path, compiler)
		if err != nil {
			return err
		}
		if len(searchKeys) == 0 {
			return fmt.Errorf("schema at path %s (title: %s) must have at least one search key", path, validator.Title)
		}
		app.SchemaMap[validator.Title] = app.Schema{
			Title:      validator.Title,
			SearchKeys: searchKeys,
			Validator:  validator,
		}
	}
	return nil
}

// setSystemMap loads all system YAML files, decodes them into SystemInfo, and initializes systems in the global map.
func setSystemMap(systemFiles []string) error {
	systemInfoMap := map[string]*systems.SystemInfo{}

	for _, path := range systemFiles {
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("failed to open system file at path %s: %w", path, err)
		}
		defer f.Close()

		sysInfo := &systems.SystemInfo{}
		decoder := yaml.NewDecoder(f)
		err = decoder.Decode(sysInfo)
		if err != nil {
			return fmt.Errorf("failed to decode system file %s: %w", path, err)
		}
		if sysInfo.Name == "" {
			return fmt.Errorf("system file at path %s has no name", path)
		}
		systemInfoMap[sysInfo.Name] = sysInfo
	}

	for _, systemInfo := range systemInfoMap {

		app.Logger.Debug("initializing new system", "name", systemInfo.Name, "type", systemInfo.Type)

		app.ObjectQueue.SetSafeIndex(systemInfo.Name, 0, systemInfo.Name)

		// Initialize each system and store in the global map
		system, err := systems.NewSystem(systemInfo)
		if err != nil {
			app.Logger.Error("failed to initialize system", "error", err)
			os.Exit(1)
		}

		systems.SystemMap[systemInfo.Name] = system
	}

	return nil
}
