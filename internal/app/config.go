package app

type config struct {
	Port               int
	ConfigDir          string
	DuplicateCacheSize int
	LogLevel           string
	DisplayVersion     bool
}

var Config = &config{}
