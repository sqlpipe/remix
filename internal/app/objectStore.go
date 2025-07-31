package app

import (
	"sync"
)

type Object struct {
	Schema    string         `json:"schema"`
	Operation string         `json:"operation"`
	Payload   map[string]any `json:"payload"`
}

type objectQueue struct {
	safeIndexMap map[string]int64
	indexMapMu   sync.RWMutex
	safeObjects  []*Object
	objectsMu    sync.RWMutex
}

var ObjectQueue = &objectQueue{
	safeIndexMap: make(map[string]int64),
	safeObjects:  make([]*Object, 0),
}

func (s *objectQueue) GetSafeIndex(key string, systemName string) (int64, bool) {
	s.indexMapMu.RLock()
	defer s.indexMapMu.RUnlock()
	index, exists := s.safeIndexMap[key]

	if Config.LogLevel == "debug" {
		AddToDebugStore(DebugMessage{Payload: index, Operation: "Got safe index map", System: systemName})
	}

	return index, exists
}

func (s *objectQueue) SetSafeIndex(key string, index int64, systemName string) {
	s.indexMapMu.Lock()
	defer s.indexMapMu.Unlock()
	s.safeIndexMap[key] = index

	if Config.LogLevel == "debug" {
		AddToDebugStore(DebugMessage{Payload: index, Operation: "Set safe index map", System: systemName})
	}
}

func (s *objectQueue) GetSafeObjectsFromIndex(index int64, systemName string) []*Object {
	s.objectsMu.RLock()
	defer s.objectsMu.RUnlock()

	if index < 0 || index >= int64(len(s.safeObjects)) {
		return nil
	}

	objects := s.safeObjects[index:]

	if Config.LogLevel == "debug" {
		AddToDebugStore(DebugMessage{Payload: objects, Operation: "Got from queue", System: systemName})
	}

	return objects
}

func (s *objectQueue) AddSafeObject(object *Object, systemName string) {
	s.objectsMu.Lock()
	defer s.objectsMu.Unlock()
	s.safeObjects = append(s.safeObjects, object)

	if Config.LogLevel == "debug" {
		AddToDebugStore(DebugMessage{Payload: object, Operation: "Added to queue", System: systemName})
	}
}
