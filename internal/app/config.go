package app

type config struct {
	Port               int
	ConfigDir          string
	DuplicateCacheSize int
	LogLevel           string
	DisplayVersion     bool
	Limiter            struct {
		Enabled bool
		Rps     float64
		Burst   int
	}
	MaxSnapshots int // Maximum number of object store snapshots to keep in debug mode
	MaxRAMSizeMB int // Maximum RAM size for seen objects, in MB
}

var Config = &config{}
