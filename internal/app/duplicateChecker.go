package app

import (
	"fmt"
	"sync"
)

type lastSeenObjects map[any]Object

type searchKeys map[string]lastSeenObjects

type duplicateChecker struct {
	Schemas map[string]searchKeys
	Mu      sync.RWMutex
}

var DuplicateChecker = duplicateChecker{
	Schemas: make(map[string]searchKeys),
	Mu:      sync.RWMutex{},
}

func (d *duplicateChecker) ensureSchemaInitialized(schemaName string) error {
	_, exists := d.Schemas[schemaName]
	if !exists {
		d.Schemas[schemaName] = make(searchKeys)

		schema, ok := SchemaMap[schemaName]
		if !ok {
			err := fmt.Errorf("no schema found for object: %s", schemaName)
			Logger.Error(err.Error(), "schemaName", schemaName)
			return err
		}

		for _, key := range schema.SearchKeys {
			d.Schemas[schemaName][key] = make(lastSeenObjects)
		}
	}
	return nil
}

func (d *duplicateChecker) AddObject(object *Object) {
	d.Mu.Lock()
	defer d.Mu.Unlock()

	err := d.ensureSchemaInitialized(object.Schema)
	if err != nil {
		return
	}

	for _, key := range SchemaMap[object.Schema].SearchKeys {
		d.Schemas[object.Schema][key][object.Payload[key]] = *object
	}
}

func (d *duplicateChecker) CheckIfSeen(object *Object) bool {
	d.Mu.RLock()
	defer d.Mu.RUnlock()

	for _, key := range SchemaMap[object.Schema].SearchKeys {
		searchVal := object.Payload[key]

		lastSeenObject, exists := d.Schemas[object.Schema][key][searchVal]
		if !exists {
			continue
		}

		if lastSeenObject.Operation != object.Operation {
			continue
		}

		lastSeenContainsAllInfo := true

		for k, v := range object.Payload {
			if v != lastSeenObject.Payload[k] {
				lastSeenContainsAllInfo = false
				break
			}
		}

		if lastSeenContainsAllInfo {
			return true
		}
	}
	return false
}
