package main

import (
	"database/sql"
	"fmt"
	"net/http"

	_ "github.com/snowflakedb/gosnowflake"
)

type Snowflake struct {
	Connection *sql.DB
}

func newSnowflake(systemInfo SystemInfo) (snowflake Snowflake, err error) {
	db, err := openConnectionPool(systemInfo.Name, systemInfo.ConnectionString, DriverSnowflake)
	if err != nil {
		return snowflake, fmt.Errorf("error opening snowflake db :: %v", err)
	}

	snowflake.Connection = db

	return snowflake, nil
}

func (s Snowflake) handleWebhook(w http.ResponseWriter, r *http.Request) {
	// Snowflake will not send us webhooks, so this is a no-op
}
