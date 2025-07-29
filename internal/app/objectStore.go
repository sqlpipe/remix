package app

import (
	"sync"
)

type Object struct {
	Type      string         `json:"type"`
	Operation string         `json:"operation"`
	Payload   map[string]any `json:"payload"`
}

type objectStore struct {
	safeIndexMap map[string]int64
	indexMapMu   sync.RWMutex
	safeObjects  []Object
	objectsMu    sync.RWMutex
}

var ObjectStore = &objectStore{
	safeIndexMap: make(map[string]int64),
	safeObjects:  make([]Object, 0),
}

func (s *objectStore) GetSafeIndexMap(key string) (int64, bool) {
	s.indexMapMu.RLock()
	defer s.indexMapMu.RUnlock()
	index, exists := s.safeIndexMap[key]
	return index, exists
}

func (s *objectStore) SetSafeIndexMap(key string, index int64) {
	s.indexMapMu.Lock()
	defer s.indexMapMu.Unlock()
	s.safeIndexMap[key] = index
}

func (s *objectStore) GetSafeObjectsFromIndex(index int64) []Object {
	s.objectsMu.RLock()
	defer s.objectsMu.RUnlock()

	if index < 0 || index >= int64(len(s.safeObjects)) {
		return nil
	}

	// Return a slice of safeObjects starting from the given index
	return s.safeObjects[index:]
}

func (s *objectStore) AddSafeObject(object Object) {
	s.objectsMu.Lock()
	defer s.objectsMu.Unlock()
	s.safeObjects = append(s.safeObjects, object)
}

// ObjectStoreState represents a snapshot of the object store's state for debugging or inspection.
type ObjectStoreState struct {
	SafeIndexMap map[string]int64 `json:"safe_index_map"`
	SafeObjects  []Object         `json:"safe_objects"`
}
