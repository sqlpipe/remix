package systems

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"time"
)

var SystemMap = make(map[string]SystemInterface)

var (
	Statuses = []string{StatusQueued, StatusRunning, StatusCancelled, StatusError, StatusComplete, ""}

	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusCancelled = "cancelled"
	StatusError     = "error"
	StatusComplete  = "complete"

	TypePostgreSQL = "postgresql"
	TypeMySQL      = "mysql"
	TypeMSSQL      = "mssql"
	TypeOracle     = "oracle"
	TypeSnowflake  = "snowflake"
	TypeStripe     = "stripe"

	DriverPostgreSQL = "pgx"
	DriverMySQL      = "mysql"
	DriverMSSQL      = "sqlserver"
	DriverOracle     = "oracle"
	DriverSnowflake  = "snowflake"
)

type Field struct {
	Field     string `yaml:"field" json:"field"`
	SearchKey bool   `yaml:"search_key,omitempty" json:"search_key,omitempty"`
	// Hardcode  any    `yaml:"hardcode,omitempty" json:"hardcode,omitempty"`
}

type ReceiveMixer map[string]PullLocation
type PullLocation map[string]PullSchema
type PullSchema map[string]Field

type PushMixer map[string]PushSchema
type PushSchema map[string]PushLocation
type PushLocation struct {
	Fields     map[string]Field `yaml:"fields" json:"fields"`
	SearchKeys []string         `yaml:"search_keys" json:"search_keys"`
}

type SystemInfo struct {
	Name                string        `yaml:"name" json:"name"`
	Type                string        `yaml:"type" json:"type"`
	ConnectionString    string        `yaml:"dsn" json:"dsn"`
	MaxOpenConnections  int           `yaml:"max_open_connections" json:"max_open_connections"`
	MaxIdleConnections  int           `yaml:"max_idle_connections" json:"max_idle_connections"`
	MaxIdleTime         time.Duration `yaml:"max_connection_idle_time" json:"max_connection_idle_time"`
	Hostname            string        `yaml:"hostname,omitempty" json:"hostname,omitempty"`
	Port                int           `yaml:"port,omitempty" json:"port,omitempty"`
	Database            string        `yaml:"database,omitempty" json:"database,omitempty"`
	Username            string        `yaml:"username,omitempty" json:"username,omitempty"`
	Password            string        `yaml:"-" json:"-"`
	Dsn                 string        `yaml:"-" json:"-"`
	ReplicationDsn      string        `yaml:"replication_dsn,omitempty" json:"replication_dsn,omitempty"`
	ApiKey              string        `yaml:"api_key" json:"-"`
	EndpointSecret      string        `yaml:"-" json:"-"`
	RateLimit           int           `yaml:"rate_limit,omitempty" json:"rate_limit,omitempty"`
	RateBucketSize      int           `yaml:"rate_bucket_size,omitempty" json:"rate_bucket_size,omitempty"`
	UseCliListener      bool          `yaml:"use_cli_listener,omitempty" json:"use_cli_listener,omitempty"`
	ReceiveMixer        *ReceiveMixer `yaml:"receive_mixer,omitempty" json:"receive_mixer,omitempty"`
	PushMixer           *PushMixer    `yaml:"push_mixer,omitempty" json:"push_mixer,omitempty"`
	ReplicationSlotName string        `yaml:"replication_slot_name,omitempty" json:"replication_slot_name,omitempty"`
	PublicationName     string        `yaml:"publication_name,omitempty" json:"publication_name,omitempty"`
}

type SystemInterface interface {
	HandleWebhook(w http.ResponseWriter, r *http.Request)
}

func NewSystem(systemInfo *SystemInfo) (system SystemInterface, err error) {
	switch systemInfo.Type {
	case TypePostgreSQL:
		return newPostgresql(systemInfo)
	case TypeSnowflake:
		return newSnowflake(systemInfo)
	case TypeStripe:
		return newStripe(systemInfo)
	default:
		return system, fmt.Errorf("unsupported system type %v", systemInfo.Type)
	}
}

func openConnectionPool(name, connectionString, driverName string) (connectionPool *sql.DB, err error) {

	connectionPool, err = sql.Open(driverName, connectionString)
	if err != nil {
		return nil, fmt.Errorf("error opening connection to %v :: %v", name, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = connectionPool.PingContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("error pinging %v :: %v", name, err)
	}

	return connectionPool, nil
}
