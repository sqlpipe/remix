package systems

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/sqlpipe/remix/internal/app"
	"golang.org/x/time/rate"
)

type Postgresql struct {
	db         *sql.DB
	replConn   *pgconn.PgConn
	systemInfo SystemInfo
	limiter    *rate.Limiter
}

func newPostgresql(systemInfo SystemInfo) (*Postgresql, error) {

	if len(systemInfo.PushMixer) == 0 && len(systemInfo.ReceiveMixer) == 0 {
		return nil, fmt.Errorf("systemInfo must have at least one of PushMixer or ReceiveMixer configured")
	}

	postgresql, err := createPostgreSQLStruct(systemInfo)
	if err != nil {
		return nil, err
	}

	if len(systemInfo.ReceiveMixer) == 0 {
		go postgresql.loop(0)
		app.Logger.Info("PostgreSQL initialized in push-only mode (no CDC)", "system", systemInfo.Name)
		return &postgresql, nil
	}

	postgresql, err = initializeCDCMode(postgresql.db, postgresql.replConn, systemInfo, postgresql)
	if err != nil {
		return nil, err
	}

	return &postgresql, nil
}

// Helper function to initialize CDC mode for PostgreSQL
func initializeCDCMode(db *sql.DB, replConn *pgconn.PgConn, systemInfo SystemInfo, postgresql Postgresql) (Postgresql, error) {
	tableList, err := setupPublication(db, systemInfo.PublicationName, systemInfo.ReceiveMixer)
	if err != nil {
		return postgresql, fmt.Errorf("error setting up publication: %v", err)
	}

	sysident, err := setupReplicationSlot(replConn, systemInfo.ReplicationSlotName)
	if err != nil {
		return postgresql, fmt.Errorf("error setting up replication slot: %v", err)
	}

	app.Logger.Info("PostgreSQL initialized in CDC mode",
		"system", systemInfo.Name,
		"publication", systemInfo.PublicationName,
		"tables", tableList,
		"slot", systemInfo.ReplicationSlotName,
	)

	pluginArguments := []string{}

	if app.Config.LogLevel == "debug" {
		pluginArguments = append(pluginArguments, "\"pretty-print\" 'true'")
	}

	err = pglogrepl.StartReplication(context.Background(), replConn, systemInfo.ReplicationSlotName, sysident.XLogPos,
		pglogrepl.StartReplicationOptions{
			PluginArgs: pluginArguments,
		})
	if err != nil {
		app.Logger.Error("StartReplication failed", "error", err)
		os.Exit(1)
	}

	go postgresql.loop(sysident.XLogPos)

	return postgresql, nil
}

// Helper to create PostgreSQL struct with connections
func createPostgreSQLStruct(systemInfo SystemInfo) (Postgresql, error) {
	db, err := openConnectionPool(systemInfo.Name, systemInfo.ConnectionString, DriverPostgreSQL)
	if err != nil {
		return Postgresql{}, fmt.Errorf("error opening PostgreSQL connection pool :: %v", err)
	}

	app.Logger.Info("PostgreSQL connection established", "system", systemInfo.Name)

	replConn, err := pgconn.Connect(context.Background(), systemInfo.ReplicationDsn)
	if err != nil {
		return Postgresql{}, fmt.Errorf("error opening postgresql replication connection :: %v", err)
	}

	postgresql := Postgresql{
		db:         db,
		replConn:   replConn,
		systemInfo: systemInfo,
		limiter:    rate.NewLimiter(rate.Limit(systemInfo.RateLimit), systemInfo.RateBucketSize),
	}

	return postgresql, nil
}

// Helper to setup publication
func setupPublication(db *sql.DB, pubName string, receiveMixer ReceiveMixer) ([]string, error) {
	if pubName == "" {
		return nil, fmt.Errorf("publication_name must be set in yaml config file")
	}
	dropPubSQL := fmt.Sprintf("DROP PUBLICATION IF EXISTS %s;", pubName)
	_, _ = db.Exec(dropPubSQL)

	tableSet := make(map[string]struct{})
	for table := range receiveMixer {
		tableSet[table] = struct{}{}
	}
	tableList := make([]string, 0, len(tableSet))
	for table := range tableSet {
		tableList = append(tableList, table)
	}

	if len(tableList) == 0 {
		return tableList, nil // push-only mode
	}

	createPubSQL := ""
	if len(tableList) > 0 {
		createPubSQL = fmt.Sprintf("CREATE PUBLICATION %s FOR TABLE %s;", pubName, strings.Join(tableList, ", "))
	} else {
		createPubSQL = fmt.Sprintf("CREATE PUBLICATION %s FOR ALL TABLES;", pubName)
	}
	_, err := db.Exec(createPubSQL)
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return nil, fmt.Errorf("failed to create publication: %v", err)
	}
	return tableList, nil
}

// Helper to setup replication slot
func setupReplicationSlot(replConn *pgconn.PgConn, slotName string) (sysident pglogrepl.IdentifySystemResult, err error) {
	if slotName == "" {
		return sysident, fmt.Errorf("replication_slot_name must be set in systemInfo")
	}
	sysident, err = pglogrepl.IdentifySystem(context.Background(), replConn)
	if err != nil {
		return sysident, fmt.Errorf("IdentifySystem failed: %v", err)
	}
	_, err = pglogrepl.CreateReplicationSlot(context.Background(), replConn, slotName, "wal2json", pglogrepl.CreateReplicationSlotOptions{Temporary: false, Mode: pglogrepl.LogicalReplication})
	if err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			return sysident, fmt.Errorf("CreateReplicationSlot failed: %v", err)
		}
	}
	return sysident, nil
}

// loop continuously processes safe objects from the ObjectQueue and applies them to PostgreSQL.
// It also manages logical replication by handling WAL (Write-Ahead Log) messages from PostgreSQL using the replication connection.
// The function:
//   - Applies upsert/delete operations to the database for new objects, avoiding duplicates.
//   - Sends periodic standby status updates to the PostgreSQL server.
//   - Receives and processes logical replication messages (CDC events) to keep the system in sync.
//   - Uses a rate limiter to control the pace of operations.
//
// This function is designed to run as a goroutine and will panic if the system is misconfigured or encounters fatal errors.
func (p Postgresql) loop(startXLogPos pglogrepl.LSN) {
	clientXLogPos := startXLogPos
	standbyMessageTimeout := time.Second * 10
	nextStandbyMessageDeadline := time.Now().Add(standbyMessageTimeout)
	var index int64

	for {
		// Wait for the rate limiter to allow the next operation
		err := p.limiter.Wait(context.Background())
		if err != nil {
			app.Logger.Warn("error waiting for rate limiter", "error", err, "system", p.systemInfo.Name)
			continue
		}

		// Get the last safe object index for this system from the ObjectQueue
		var exists bool
		index, exists = app.ObjectQueue.GetSafeIndexMap(p.systemInfo.Name)
		if !exists {
			panic(fmt.Sprintf("safe index not found for system %s", p.systemInfo.Name))
		}

		// --- PUSH: Process objects from the queue ---
		if p.systemInfo.PushMixer != nil {
			prevIndex := index
			p.processPushObjects(&index)

			// Update the safe index map for this system only if index has incremented
			if index != prevIndex {
				app.ObjectQueue.SetSafeIndexMap(p.systemInfo.Name, index)
			}
		}

		// --- PULL: Process replication messages (CDC) ---
		if p.systemInfo.ReceiveMixer != nil {
			err = p.processReplicationMessage(&clientXLogPos, &nextStandbyMessageDeadline)
			if err != nil {
				// If processReplicationMessage returns an error, log and break (fatal)
				app.Logger.Error("error in replication message processing", "error", err)
				return
			}
		}
	}
}

// processPushObjects processes safe objects from the ObjectQueue and applies them to PostgreSQL.
func (p Postgresql) processPushObjects(index *int64) {
	objects := app.ObjectQueue.GetSafeObjectsFromIndex(*index)
	if len(objects) > 0 {
		*index += int64(len(objects))
		if app.Config.LogLevel == "debug" {
			app.AddToDebugStore(app.DebugMessage{Payload: objects, Operation: "Got from queue", System: p.systemInfo.Name})
		}
	}

	for _, object := range objects {

		newObjects := applyPushMixer(object, p.systemInfo.PushMixer)

		for _, newObject := range newObjects {
			foundDuplicate := app.DuplicateChecker.CheckIfSeen(&newObject)
			if !foundDuplicate {

				for locationInSystem := range p.systemInfo.PushMixer[object.Schema] {

					switch newObject.Operation {
					case "upsert":
						err := p.upsertObject(newObject.Payload, locationInSystem, newObject.Schema, &newObject)
						if err != nil {
							app.Logger.Error("error upserting JSON to PostgreSQL", "error", err, "objectType", object.Schema, "locationInSystem", locationInSystem, "data", object)
						}
					case "delete":
						err := p.deleteFromPostgresql(newObject.Payload, searchFields, locationInSystem, &object)
						if err != nil {
							app.Logger.Error("error deleting from PostgreSQL", "error", err, "objectType", object.Schema, "locationInSystem", locationInSystem, "data", newObject)
						}
					}
				}
			}
		}
	}
}

// processReplicationMessage handles standby status updates and incoming replication (CDC) messages.
func (p Postgresql) processReplicationMessage(clientXLogPos *pglogrepl.LSN, nextStandbyMessageDeadline *time.Time) error {
	standbyMessageTimeout := time.Second * 10
	// Send a standby status update to the replication connection if needed
	if time.Now().After(*nextStandbyMessageDeadline) {
		err := pglogrepl.SendStandbyStatusUpdate(context.Background(), p.replConn, pglogrepl.StandbyStatusUpdate{WALWritePosition: *clientXLogPos})
		if err != nil {
			return fmt.Errorf("SendStandbyStatusUpdate failed: %v", err)
		}
		*nextStandbyMessageDeadline = time.Now().Add(standbyMessageTimeout)
	}

	ctx, cancel := context.WithDeadline(context.Background(), *nextStandbyMessageDeadline)
	rawMsg, err := p.replConn.ReceiveMessage(ctx)
	cancel()
	if err != nil {
		if pgconn.Timeout(err) {
			return nil // Not fatal, just continue
		}
		return fmt.Errorf("ReceiveMessage failed: %v", err)
	}

	if errMsg, ok := rawMsg.(*pgproto3.ErrorResponse); ok {
		return fmt.Errorf("received Postgres WAL error: %+v", errMsg)
	}

	msg, ok := rawMsg.(*pgproto3.CopyData)
	if !ok {
		log.Printf("Received unexpected message: %T\n", rawMsg)
		return nil // Not fatal, just continue
	}

	switch msg.Data[0] {
	case pglogrepl.PrimaryKeepaliveMessageByteID:
		pkm, err := pglogrepl.ParsePrimaryKeepaliveMessage(msg.Data[1:])
		if err != nil {
			return fmt.Errorf("ParsePrimaryKeepaliveMessage failed: %v", err)
		}
		if pkm.ServerWALEnd > *clientXLogPos {
			*clientXLogPos = pkm.ServerWALEnd
		}
		if pkm.ReplyRequested {
			*nextStandbyMessageDeadline = time.Time{}
		}
	case pglogrepl.XLogDataByteID:
		xld, err := pglogrepl.ParseXLogData(msg.Data[1:])
		if err != nil {
			return fmt.Errorf("ParseXLogData failed: %v", err)
		}
		err = p.handleCdcEvent(string(xld.WALData))
		if err != nil {
			return fmt.Errorf("error handling CDC event: %v", err)
		}
		if xld.WALStart > *clientXLogPos {
			*clientXLogPos = xld.WALStart
		}
	default:
		// Ignore other message types
	}
	return nil
}

func (p Postgresql) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	app.Logger.Error("PostgreSQL does not support webhooks", "system", p.systemInfo.Name)
}

func (p Postgresql) upsertObject(object app.Object) error {

	searchKeys := []string{}

	for _, field := range app.SchemaMap[object.Schema].SearchKeys {
		_, ok := object.Payload[field]
		if ok {
			searchKeys = append(searchKeys, field)
		}
	}

	for locationInSystem := range p.systemInfo.PushMixer[object.Schema] {

		var foundMatch bool
		var conflictField string
		var conflictValue any

		// Check if a row exists with this search field
		for _, field := range searchKeys {

			v := object.Payload[field]

			query := fmt.Sprintf("SELECT 1 FROM %s WHERE %s = $1 LIMIT 1", locationInSystem, field)
			row := p.db.QueryRow(query, v)
			var dummy int
			err := row.Scan(&dummy)
			if err == nil {
				foundMatch = true
				conflictField = field
				conflictValue = v
				break
			}
			// if err != sql.ErrNoRows && err != nil {
			if err != sql.ErrNoRows {
				app.Logger.Error("error checking for existing row", "error", err, "query", query, "value", v)
				return fmt.Errorf("error checking for existing row: %v", err)
			}

			if foundMatch {
				// Prepare UPDATE: set all columns except the conflict field
				setCols := make([]string, 0, len(data))
				values := make([]any, 0, len(data))
				idx := 1
				for k, v := range data {
					if k != conflictField {
						setCols = append(setCols, fmt.Sprintf("%s = $%d", k, idx))
						values = append(values, v)
						idx++
					}
				}
				// Add WHERE for the conflict field
				whereClause := fmt.Sprintf("%s = $%d", conflictField, idx)
				values = append(values, conflictValue)

				updateQuery := fmt.Sprintf(
					"UPDATE %s SET %s WHERE %s",
					locationInSystem,
					strings.Join(setCols, ", "),
					whereClause,
				)

				_, err := p.db.Exec(updateQuery, values...)
				if err != nil {
					app.Logger.Error("error executing update query", "error", err, "query", updateQuery, "values", values)
					return fmt.Errorf("error executing update query: %v", err)
				}
			} else {
				// Build INSERT
				columns := make([]string, 0, len(data))
				placeholders := make([]string, 0, len(data))
				insertValues := make([]any, 0, len(data))

				idx := 1
				for k, v := range data {
					columns = append(columns, k)
					placeholders = append(placeholders, fmt.Sprintf("$%d", idx))
					insertValues = append(insertValues, v)
					idx++
				}
				insertQuery := fmt.Sprintf(
					"INSERT INTO %s (%s) VALUES (%s)",
					locationInSystem,
					strings.Join(columns, ", "),
					strings.Join(placeholders, ", "),
				)
				_, err := p.db.Exec(insertQuery, insertValues...)
				if err != nil {
					app.Logger.Error("error executing insert query", "error", err, "query", insertQuery, "values", insertValues)
					return fmt.Errorf("error executing insert query: %v", err)
				}
			}
		}
	}

	object := &app.Object{
		Schema:    objectSchema,
		Operation: "upsert",
		Payload:   data,
	}
	app.DuplicateChecker.AddObject(object)

	return nil
}

type OldKeys struct {
	KeyNames  []string `json:"keynames"`
	KeyValues []any    `json:"keyvalues,omitempty"` // Optional, if not provided, the old keys are not included
}

type CdcChange struct {
	Kind         string   `json:"kind"`
	Schema       string   `json:"schema"`
	Table        string   `json:"table"`
	ColumnNames  []string `json:"columnnames"`
	ColumnTypes  []string `json:"columntypes"`
	ColumnValues []any    `json:"columnvalues"`
	OldKeys      OldKeys  `json:"oldkeys,omitempty"`
}

type CdcEvent struct {
	Change []CdcChange `json:"change"`
}

func (p Postgresql) handleCdcEvent(jsonString string) error {

	var event CdcEvent
	err := json.Unmarshal([]byte(jsonString), &event)
	if err != nil {
		return fmt.Errorf("error unmarshalling CDC event: %v", err)
	}

	if app.Config.LogLevel == "debug" {
		app.AddToDebugStore(app.DebugMessage{Payload: event, Operation: "Received CDC events", System: p.systemInfo.Name})
	}

	for _, change := range event.Change {
		pullLocation := change.Schema + "." + change.Table
		operationType := change.Kind

		obj := map[string]any{}
		newObjs := make(map[string]map[string]any)

		switch operationType {
		case "insert", "update":
			operationType = "upsert"
			for i, colName := range change.ColumnNames {
				if change.ColumnValues[i] != nil {
					obj[colName] = change.ColumnValues[i]
				}
			}
		case "delete":
			operationType = "delete"
			for i, colName := range change.OldKeys.KeyNames {
				if change.OldKeys.KeyValues != nil {
					obj[colName] = change.OldKeys.KeyValues[i]
				}
			}
		default:
			return fmt.Errorf("unknown operation type: %s", operationType)
		}

		for objectType, pullObject := range p.systemInfo.ReceiveMixer[pullLocation] {
			newObj := map[string]any{}

			for keyInObj, fields := range pullObject {
				newObj[fields.Field] = obj[keyInObj]
			}

			newObjs[objectType] = newObj
		}

		for schemaName, obj := range newObjs {

			for k, v := range obj {
				if v == nil {
					delete(obj, k)
				}
			}

			schema, inMap := app.SchemaMap[schemaName]
			if !inMap {
				return fmt.Errorf("no schema found for pull location: %s", pullLocation)
			}

			err = schema.Validator.Validate(obj)
			if err != nil {
				return fmt.Errorf("object failed postgresql schema validation for '%s': %v", pullLocation, err)
			}

			var objectIsDuplicate bool
			foundDuplicate := false
			for i, expiringObj := range p.duplicateChecker[schemaName] {

				objectIsDuplicate = true

				if expiringObj.Operation != operationType {
					objectIsDuplicate = false
				} else {
					if operationType == "delete" {
						for k, v := range obj {
							if v != expiringObj.Payload[k] {
								objectIsDuplicate = false
								break
							}
						}
					} else if operationType == "upsert" {
						for k, v := range expiringObj.Payload {
							if v != obj[k] {
								objectIsDuplicate = false
								break
							}
						}
					}
				}

				if objectIsDuplicate {
					// If we found a duplicate, we can remove it from the duplicate checker
					p.duplicateChecker[schemaName] = append(p.duplicateChecker[schemaName][:i], p.duplicateChecker[schemaName][i+1:]...)
					foundDuplicate = true
					break
				}
			}

			if !foundDuplicate {

				object := app.Object{
					Operation: operationType,
					Schema:    schemaName,
					Payload:   obj,
				}

				// also add to storage engine
				app.ObjectQueue.AddSafeObject(object)

				if app.Config.LogLevel == "debug" {
					app.AddToDebugStore(app.DebugMessage{Payload: object, Operation: "Adding to queue", System: p.systemInfo.Name})
				}

				expiringObj := app.Object{
					Operation: operationType,
					Schema:    schemaName,
					Payload:   obj,
				}
				p.duplicateChecker[schemaName] = append(p.duplicateChecker[schemaName], &expiringObj)

			}
		}
	}

	return nil
}

// deleteFromPostgresql deletes a row from PostgreSQL based on the searchFields and payload.
func (p Postgresql) deleteFromPostgresql(payload map[string]any, searchFields []string, locationInSystem string, originalObj *app.Object) error {

	if len(searchFields) == 0 {
		return fmt.Errorf("no search fields provided for delete operation")
	}

	if app.Config.LogLevel == "debug" {
		app.AddToDebugStore(app.DebugMessage{Payload: payload, Operation: "Deleting from", System: p.systemInfo.Name})
	}

	whereClauses := make([]string, 0, len(searchFields))
	values := make([]any, 0, len(searchFields))
	idx := 1
	for _, field := range searchFields {
		val, ok := payload[field]
		if !ok {
			return fmt.Errorf("search field '%s' not found in payload", field)
		}
		whereClauses = append(whereClauses, fmt.Sprintf("%s = $%d", field, idx))
		values = append(values, val)
		idx++
	}

	deleteQuery := fmt.Sprintf("DELETE FROM %s WHERE %s", locationInSystem, strings.Join(whereClauses, " AND "))
	_, err := p.db.Exec(deleteQuery, values...)
	if err != nil {
		app.Logger.Error("error executing delete query", "error", err, "query", deleteQuery, "values", values)
		return fmt.Errorf("error executing delete query: %v", err)
	}

	expiringObj := app.Object{
		Operation: "delete",
		Schema:    originalObj.Schema,
		Payload:   payload,
	}
	p.duplicateChecker[originalObj.Schema] = append(p.duplicateChecker[originalObj.Schema], &expiringObj)

	return nil
}
