package systems

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
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
	db               *sql.DB
	replConn         *pgconn.PgConn
	systemInfo       SystemInfo
	limiter          *rate.Limiter
	duplicateChecker map[string][]*app.Object
	logger           *slog.Logger
}

func newPostgresql(systemInfo SystemInfo) (postgresql Postgresql, err error) {
	db, err := openConnectionPool(systemInfo.Name, systemInfo.ConnectionString, DriverPostgreSQL)
	if err != nil {
		return postgresql, fmt.Errorf("error opening PostgreSQL connection pool :: %v", err)
	}

	app.Logger.Info("PostgreSQL connection established", "system", systemInfo.Name)

	// Create replication connection
	replConn, err := pgconn.Connect(context.Background(), systemInfo.ReplicationDsn)
	if err != nil {
		return postgresql, fmt.Errorf("error opening postgresql replication connection :: %v", err)
	}

	postgresql.db = db
	postgresql.replConn = replConn
	postgresql.systemInfo = systemInfo
	postgresql.limiter = rate.NewLimiter(rate.Limit(systemInfo.RateLimit), systemInfo.RateBucketSize)
	postgresql.duplicateChecker = make(map[string][]*app.Object)

	for schemaName := range app.SchemaMap {
		postgresql.duplicateChecker[schemaName] = make([]*app.Object, 0)
	}

	go postgresql.loop()
	go postgresql.watchCDC()

	return postgresql, nil
}

func (p Postgresql) loop() {

	slotName := "sqlpipe_slot"
	outputPlugin := "wal2json"

	replConn, err := pgconn.Connect(context.Background(), p.systemInfo.ReplicationDsn)
	if err != nil {
		p.logger.Error("failed to connect", "error", err)
		os.Exit(1)
	}
	defer replConn.Close(context.Background())

	sysident, err := pglogrepl.IdentifySystem(context.Background(), replConn)
	if err != nil {
		p.logger.Error("IdentifySystem failed", "error", err)
		os.Exit(1)
	}

	_, err = pglogrepl.CreateReplicationSlot(context.Background(), replConn, slotName, outputPlugin, pglogrepl.CreateReplicationSlotOptions{Temporary: false, Mode: pglogrepl.LogicalReplication})
	if err != nil {
		// If the error is "already exists", it's OK, otherwise fail
		if !strings.Contains(err.Error(), "already exists") {
			p.logger.Error("CreateReplicationSlot failed", "error", err)
			os.Exit(1)
		}
	}

	pluginArguments := []string{"\"pretty-print\" 'true'"}
	err = pglogrepl.StartReplication(context.Background(), replConn, slotName, sysident.XLogPos,
		pglogrepl.StartReplicationOptions{
			PluginArgs: pluginArguments,
		})
	if err != nil {
		p.logger.Error("StartReplication failed", "error", err)
		os.Exit(1)
	}

	clientXLogPos := sysident.XLogPos
	standbyMessageTimeout := time.Second * 10
	nextStandbyMessageDeadline := time.Now().Add(standbyMessageTimeout)

	var index int64
	for {
		// Get the last safe object index for this system
		var exists bool
		index, exists = app.ObjectStore.GetSafeIndexMap(p.systemInfo.Name)
		if !exists {
			panic(fmt.Sprintf("safe index not found for system %s", p.systemInfo.Name))
		}

		// Wait for rate limiter
		err := p.limiter.Wait(context.Background())
		if err != nil {
			// Optionally log or handle error, then break or continue
			continue
		}

		// Query safeObjects after lastIndex
		objects := app.ObjectStore.GetSafeObjectsFromIndex(index)
		if len(objects) > 0 {
			// Process new objects as needed
			index += int64(len(objects))
		}

		for _, object := range objects {

			b, _ := json.MarshalIndent(object, "", "  ")
			p.logger.Debug("PostgreSQL got from queue", "object", string(b))

			searchFields := []string{}

			for locationInSystem, fields := range p.systemInfo.PushMixer[object.Type] {
				newObj := app.Object{
					Operation: object.Operation,
					Type:      object.Type,
					Payload:   make(map[string]any),
				}
				for keyInSchema, location := range fields {
					if _, ok := object.Payload[keyInSchema]; ok {
						newObj.Payload[location.Field] = object.Payload[keyInSchema]

						if fields[keyInSchema].SearchKey {
							searchFields = append(searchFields, location.Field)
						}
					}
				}

				var objectIsDuplicate bool
				foundDuplicate := false
				for i, object := range p.duplicateChecker[object.Type] {

					objectIsDuplicate = true

					if object.Operation != object.Operation {
						objectIsDuplicate = false
					} else {
						for k, v := range newObj.Payload {
							if _, ok := object.Payload[k]; !ok {
								objectIsDuplicate = false
								break
							}
							if v != object.Payload[k] {
								objectIsDuplicate = false
								break
							}
						}
					}

					if objectIsDuplicate {
						// If we found a duplicate, we can remove it from the duplicate checker
						p.duplicateChecker[object.Type] = append(p.duplicateChecker[object.Type][:i], p.duplicateChecker[object.Type][i+1:]...)
						foundDuplicate = true
						break
					}
				}

				p.logger.Debug("PostgreSQL duplicate check result", "isDuplicate", objectIsDuplicate, "object", newObj)

				if !foundDuplicate {
					switch newObj.Operation {
					case "upsert":
						err = p.upsertJSON(newObj.Payload, searchFields, locationInSystem, newObj.Type, &object)
						if err != nil {
							p.logger.Error("error upserting JSON to PostgreSQL", "error", err, "objectType", object.Type, "locationInSystem", locationInSystem, "data", object)
						}
					case "delete":
						err = p.deleteFromPostgresql(newObj.Payload, searchFields, locationInSystem, &object)
						if err != nil {
							p.logger.Error("error deleting from PostgreSQL", "error", err, "objectType", object.Type, "locationInSystem", locationInSystem, "data", newObj)
						}
					}
				}

			}
		}

		// Update the safe index map for this system
		app.ObjectStore.SetSafeIndexMap(p.systemInfo.Name, index)

		if time.Now().After(nextStandbyMessageDeadline) {
			err = pglogrepl.SendStandbyStatusUpdate(context.Background(), replConn, pglogrepl.StandbyStatusUpdate{WALWritePosition: clientXLogPos})
			if err != nil {
				log.Fatalln("SendStandbyStatusUpdate failed:", err)
			}
			nextStandbyMessageDeadline = time.Now().Add(standbyMessageTimeout)
		}

		ctx, cancel := context.WithDeadline(context.Background(), nextStandbyMessageDeadline)
		rawMsg, err := replConn.ReceiveMessage(ctx)
		cancel()
		if err != nil {
			if pgconn.Timeout(err) {
				continue
			}
			log.Fatalln("ReceiveMessage failed:", err)
		}

		if errMsg, ok := rawMsg.(*pgproto3.ErrorResponse); ok {
			log.Fatalf("received Postgres WAL error: %+v", errMsg)
		}

		msg, ok := rawMsg.(*pgproto3.CopyData)
		if !ok {
			log.Printf("Received unexpected message: %T\n", rawMsg)
			continue
		}

		switch msg.Data[0] {
		case pglogrepl.PrimaryKeepaliveMessageByteID:
			pkm, err := pglogrepl.ParsePrimaryKeepaliveMessage(msg.Data[1:])
			if err != nil {
				log.Fatalln("ParsePrimaryKeepaliveMessage failed:", err)
			}
			if pkm.ServerWALEnd > clientXLogPos {
				clientXLogPos = pkm.ServerWALEnd
			}
			if pkm.ReplyRequested {
				nextStandbyMessageDeadline = time.Time{}
			}

		case pglogrepl.XLogDataByteID:
			xld, err := pglogrepl.ParseXLogData(msg.Data[1:])
			if err != nil {
				log.Fatalln("ParseXLogData failed:", err)
			}

			err = p.handleCdcEvent(string(xld.WALData))
			if err != nil {
				p.logger.Error("error handling CDC event", "error", err, "data", string(xld.WALData))
				return
			}

			if xld.WALStart > clientXLogPos {
				clientXLogPos = xld.WALStart
			}
		default:
		}
	}
}

func (p Postgresql) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	p.logger.Error("PostgreSQL does not support webhooks", "system", p.systemInfo.Name)
}

func (p Postgresql) upsertJSON(data map[string]any, searchFields []string, locationInSystem string, objectType string, originalObj *app.Object) error {

	b, _ := json.MarshalIndent(data, "", "  ")
	p.logger.Debug("Upserting to PostgreSQL", "data", string(b))

	var foundMatch bool
	var conflictField string
	var conflictValue any

	for _, field := range searchFields {
		if v, ok := data[field]; ok {
			// Check if a row exists with this search field
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
				p.logger.Error("error checking for existing row", "error", err, "query", query, "value", v)
				return fmt.Errorf("error checking for existing row: %v", err)
			}
		}
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
			p.logger.Error("error executing update query", "error", err, "query", updateQuery, "values", values)
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
			p.logger.Error("error executing insert query", "error", err, "query", insertQuery, "values", insertValues)
			return fmt.Errorf("error executing insert query: %v", err)
		}
	}

	object := &app.Object{
		Type:      objectType,
		Operation: "upsert",
		Payload:   data,
	}
	p.duplicateChecker[objectType] = append(p.duplicateChecker[objectType], object)

	b, _ = json.MarshalIndent(originalObj, "", "  ")
	p.logger.Debug("PostgreSQL added to duplicate checker", "object", string(b))

	return nil
}

// Start CDC for all tables in publication
func (p *Postgresql) watchCDC() {
	slotName := "sqlpipe_slot"
	outputPlugin := "wal2json"

	replConn, err := pgconn.Connect(context.Background(), p.systemInfo.ReplicationDsn)
	if err != nil {
		p.logger.Error("failed to connect", "error", err)
		os.Exit(1)
	}
	defer replConn.Close(context.Background())

	sysident, err := pglogrepl.IdentifySystem(context.Background(), replConn)
	if err != nil {
		p.logger.Error("IdentifySystem failed", "error", err)
		os.Exit(1)
	}

	_, err = pglogrepl.CreateReplicationSlot(context.Background(), replConn, slotName, outputPlugin, pglogrepl.CreateReplicationSlotOptions{Temporary: false, Mode: pglogrepl.LogicalReplication})
	if err != nil {
		// If the error is "already exists", it's OK, otherwise fail
		if !strings.Contains(err.Error(), "already exists") {
			p.logger.Error("CreateReplicationSlot failed", "error", err)
			os.Exit(1)
		}
	}

	pluginArguments := []string{"\"pretty-print\" 'true'"}
	err = pglogrepl.StartReplication(context.Background(), replConn, slotName, sysident.XLogPos,
		pglogrepl.StartReplicationOptions{
			PluginArgs: pluginArguments,
		})
	if err != nil {
		p.logger.Error("StartReplication failed", "error", err)
		os.Exit(1)
	}

	clientXLogPos := sysident.XLogPos
	standbyMessageTimeout := time.Second * 10
	nextStandbyMessageDeadline := time.Now().Add(standbyMessageTimeout)

	for {
		if time.Now().After(nextStandbyMessageDeadline) {
			err = pglogrepl.SendStandbyStatusUpdate(context.Background(), replConn, pglogrepl.StandbyStatusUpdate{WALWritePosition: clientXLogPos})
			if err != nil {
				log.Fatalln("SendStandbyStatusUpdate failed:", err)
			}
			nextStandbyMessageDeadline = time.Now().Add(standbyMessageTimeout)
		}

		ctx, cancel := context.WithDeadline(context.Background(), nextStandbyMessageDeadline)
		rawMsg, err := replConn.ReceiveMessage(ctx)
		cancel()
		if err != nil {
			if pgconn.Timeout(err) {
				continue
			}
			log.Fatalln("ReceiveMessage failed:", err)
		}

		if errMsg, ok := rawMsg.(*pgproto3.ErrorResponse); ok {
			log.Fatalf("received Postgres WAL error: %+v", errMsg)
		}

		msg, ok := rawMsg.(*pgproto3.CopyData)
		if !ok {
			log.Printf("Received unexpected message: %T\n", rawMsg)
			continue
		}

		switch msg.Data[0] {
		case pglogrepl.PrimaryKeepaliveMessageByteID:
			pkm, err := pglogrepl.ParsePrimaryKeepaliveMessage(msg.Data[1:])
			if err != nil {
				log.Fatalln("ParsePrimaryKeepaliveMessage failed:", err)
			}
			if pkm.ServerWALEnd > clientXLogPos {
				clientXLogPos = pkm.ServerWALEnd
			}
			if pkm.ReplyRequested {
				nextStandbyMessageDeadline = time.Time{}
			}

		case pglogrepl.XLogDataByteID:
			xld, err := pglogrepl.ParseXLogData(msg.Data[1:])
			if err != nil {
				log.Fatalln("ParseXLogData failed:", err)
			}

			err = p.handleCdcEvent(string(xld.WALData))
			if err != nil {
				p.logger.Error("error handling CDC event", "error", err, "data", string(xld.WALData))
				return
			}

			if xld.WALStart > clientXLogPos {
				clientXLogPos = xld.WALStart
			}
		default:
		}
	}
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

	p.logger.Debug("PostgreSQL received CDC event", "event", jsonString)

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

			err = schema.Validate(obj)
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

			p.logger.Debug("PostgreSQL duplicate check result", "isDuplicate", objectIsDuplicate, "object", obj)

			if !foundDuplicate {

				object := app.Object{
					Operation: operationType,
					Type:      schemaName,
					Payload:   obj,
				}

				// also add to storage engine
				app.ObjectStore.AddSafeObject(object)

				expiringObj := app.Object{
					Operation: operationType,
					Type:      schemaName,
					Payload:   obj,
				}
				p.duplicateChecker[schemaName] = append(p.duplicateChecker[schemaName], &expiringObj)

				p.logger.Debug("PostgreSQL added to queue and duplicate checker", "object", object)
			}
		}
	}

	return nil
}

// deleteFromPostgresql deletes a row from PostgreSQL based on the searchFields and payload.
func (p Postgresql) deleteFromPostgresql(payload map[string]any, searchFields []string, locationInSystem string, originalObj *app.Object) error {

	b, _ := json.MarshalIndent(payload, "", "  ")
	p.logger.Debug("Deleting from PostgreSQL", "payload", string(b))

	if len(searchFields) == 0 {
		return fmt.Errorf("no search fields provided for delete operation")
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
		p.logger.Error("error executing delete query", "error", err, "query", deleteQuery, "values", values)
		return fmt.Errorf("error executing delete query: %v", err)
	}

	expiringObj := app.Object{
		Operation: "delete",
		Type:      originalObj.Type,
		Payload:   payload,
	}
	p.duplicateChecker[originalObj.Type] = append(p.duplicateChecker[originalObj.Type], &expiringObj)

	b, _ = json.MarshalIndent(originalObj, "", "  ")
	p.logger.Debug("PostgreSQL added to duplicate checker", "object", string(b))

	return nil
}
