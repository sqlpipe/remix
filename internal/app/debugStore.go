package app

import (
	"sync"
)

type DebugMessage struct {
	System    string
	Operation string
	Payload   any
}

var debugStore = make([]DebugMessage, 0)
var debugStoreMu sync.RWMutex

func AddToDebugStore(msg DebugMessage) {
	debugStoreMu.Lock()
	defer debugStoreMu.Unlock()

	debugStore = append(debugStore, msg)
}

func GetDebugStore() []DebugMessage {
	debugStoreMu.RLock()
	defer debugStoreMu.RUnlock()
	return debugStore
}
