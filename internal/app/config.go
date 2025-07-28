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
}

var Config = &config{}
