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

	snapshots   []ObjectStoreState
	snapshotsMu sync.RWMutex
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
	defer s.recordSnapshotIfDebug()

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
	defer s.recordSnapshotIfDebug()

	s.objectsMu.Lock()
	defer s.objectsMu.Unlock()
	s.safeObjects = append(s.safeObjects, object)
}

// ObjectStoreState represents a snapshot of the object store's state for debugging or inspection.
type ObjectStoreState struct {
	SafeIndexMap map[string]int64 `json:"safe_index_map"`
	SafeObjects  []Object         `json:"safe_objects"`
}

// Snapshot returns a copy of the current state of the object store, safe for JSON marshalling.
func (s *objectStore) Snapshot() ObjectStoreState {
	s.indexMapMu.RLock()
	defer s.indexMapMu.RUnlock()
	s.objectsMu.RLock()
	defer s.objectsMu.RUnlock()

	// Copy the map to avoid race conditions
	indexMapCopy := make(map[string]int64, len(s.safeIndexMap))
	for k, v := range s.safeIndexMap {
		indexMapCopy[k] = v
	}
	// Copy the slice (shallow copy is fine for []Object)
	objectsCopy := make([]Object, len(s.safeObjects))
	copy(objectsCopy, s.safeObjects)

	return ObjectStoreState{
		SafeIndexMap: indexMapCopy,
		SafeObjects:  objectsCopy,
	}
}

func (s *objectStore) recordSnapshotIfDebug() {

	// Only record snapshots if in debug mode
	if Config.LogLevel != "debug" {
		return
	}

	s.snapshotsMu.Lock()
	defer s.snapshotsMu.Unlock()

	if len(s.snapshots) >= Config.MaxSnapshots {
		// Remove oldest
		s.snapshots = s.snapshots[1:]
	}

	s.snapshots = append(s.snapshots, s.Snapshot())
}

// GetSnapshotHistory returns a copy of the snapshot history (for debug endpoints)
func (s *objectStore) GetSnapshotHistory() []ObjectStoreState {
	s.snapshotsMu.RLock()
	defer s.snapshotsMu.RUnlock()
	history := make([]ObjectStoreState, 0) // Return an empty slice as snapshots are disabled
	return history
}
