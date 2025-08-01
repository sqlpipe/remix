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
	systemInfo *SystemInfo
	limiter    *rate.Limiter
}

func newPostgresql(systemInfo *SystemInfo) (*Postgresql, error) {

	err := validatePostgreSQLSystemInfo(systemInfo)
	if err != nil {
		return nil, err
	}

	postgresql, err := createPostgreSQLStruct(systemInfo)
	if err != nil {
		return nil, err
	}

	// start loop without cdc if receive mixer is nil
	if systemInfo.ReceiveMixer == nil {
		go postgresql.loop(0)
		app.Logger.Info("PostgreSQL initialized in push-only mode (no CDC)", "system", systemInfo.Name)
		return postgresql, nil
	}

	err = initializeCDCMode(postgresql.db, postgresql.replConn, systemInfo, postgresql)
	if err != nil {
		return nil, err
	}

	return postgresql, nil
}

// Helper function to initialize CDC mode for PostgreSQL
func initializeCDCMode(db *sql.DB, replConn *pgconn.PgConn, systemInfo *SystemInfo, postgresql *Postgresql) error {
	err := setupPublication(db, systemInfo.PublicationName, systemInfo.ReceiveMixer)
	if err != nil {
		return fmt.Errorf("error setting up publication: %v", err)
	}

	sysident, err := setupReplicationSlot(replConn, systemInfo.ReplicationSlotName)
	if err != nil {
		return fmt.Errorf("error setting up replication slot: %v", err)
	}

	pluginArguments := []string{}
	if app.Config.LogLevel == "debug" {
		pluginArguments = append(pluginArguments, "\"pretty-print\" 'true'")
	}

	startReplicationOptions := pglogrepl.StartReplicationOptions{PluginArgs: pluginArguments}
	err = pglogrepl.StartReplication(context.Background(), replConn, systemInfo.ReplicationSlotName, sysident.XLogPos, startReplicationOptions)
	if err != nil {
		return fmt.Errorf("StartReplication failed: %v", err)
	}

	go postgresql.loop(sysident.XLogPos)

	return nil
}

// Helper to create PostgreSQL struct with connections
func createPostgreSQLStruct(systemInfo *SystemInfo) (*Postgresql, error) {
	db, err := openConnectionPool(systemInfo.Name, systemInfo.ConnectionString, DriverPostgreSQL)
	if err != nil {
		return nil, fmt.Errorf("error opening PostgreSQL connection pool :: %v", err)
	}

	app.Logger.Info("PostgreSQL connection pool established", "system", systemInfo.Name)

	replConn, err := pgconn.Connect(context.Background(), systemInfo.ReplicationDsn)
	if err != nil {
		return nil, fmt.Errorf("error opening postgresql replication connection :: %v", err)
	}

	postgresql := &Postgresql{
		db:         db,
		replConn:   replConn,
		systemInfo: systemInfo,
		limiter:    rate.NewLimiter(rate.Limit(systemInfo.RateLimit), systemInfo.RateBucketSize),
	}

	app.Logger.Info("PostgreSQL replication connection established", "system", systemInfo.Name)

	return postgresql, nil
}

// Helper to setup publication
func setupPublication(db *sql.DB, pubName string, receiveMixer *ReceiveMixer) error {
	if pubName == "" {
		return fmt.Errorf("publication_name must be set in yaml config file")
	}
	dropPubSQL := fmt.Sprintf("DROP PUBLICATION IF EXISTS %s;", pubName)
	_, _ = db.Exec(dropPubSQL)

	tableSet := make(map[string]struct{})
	for table := range *receiveMixer {
		tableSet[table] = struct{}{}
	}
	tableList := make([]string, 0, len(tableSet))
	for table := range tableSet {
		tableList = append(tableList, table)
	}

	if len(tableList) == 0 {
		return nil // push-only mode
	}

	createPubSQL := ""
	if len(tableList) > 0 {
		createPubSQL = fmt.Sprintf("CREATE PUBLICATION %s FOR TABLE %s;", pubName, strings.Join(tableList, ", "))
	} else {
		createPubSQL = fmt.Sprintf("CREATE PUBLICATION %s FOR ALL TABLES;", pubName)
	}
	_, err := db.Exec(createPubSQL)
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return fmt.Errorf("failed to create publication: %v", err)
	}
	return nil
}

// Helper to setup replication slot
func setupReplicationSlot(replConn *pgconn.PgConn, slotName string) (*pglogrepl.IdentifySystemResult, error) {
	if slotName == "" {
		return nil, fmt.Errorf("replication_slot_name must be set in systemInfo")
	}
	sysident, err := pglogrepl.IdentifySystem(context.Background(), replConn)
	if err != nil {
		return &sysident, fmt.Errorf("IdentifySystem failed: %v", err)
	}
	_, err = pglogrepl.CreateReplicationSlot(context.Background(), replConn, slotName, "wal2json", pglogrepl.CreateReplicationSlotOptions{Temporary: false, Mode: pglogrepl.LogicalReplication})
	if err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			return &sysident, fmt.Errorf("CreateReplicationSlot failed: %v", err)
		}
	}
	return &sysident, nil
}

func (p *Postgresql) loop(xLogPos pglogrepl.LSN) {
	nextStandbyMessageDeadline := time.Now().Add(time.Second * 10)

	for {
		// Wait for the rate limiter to allow the next iteration
		err := p.limiter.Wait(context.Background())
		if err != nil {
			app.Logger.Warn("error waiting for rate limiter", "error", err, "system", p.systemInfo.Name)
			continue
		}

		// Get the last safe object index for this system from the ObjectQueue
		var exists bool
		index, exists := app.ObjectQueue.GetSafeIndex(p.systemInfo.Name)
		if !exists {
			app.Logger.Error("safe index not found for system", "system", p.systemInfo.Name)
			os.Exit(1)
		}

		if p.systemInfo.PushMixer != nil {
			index, err = p.processQueue(index)
			if err != nil {
				app.Logger.Error("error in queue processing", "error", err, "system", p.systemInfo.Name)
				continue
			}
		}

		if p.systemInfo.ReceiveMixer != nil {
			xLogPos, nextStandbyMessageDeadline, err = p.processReplicationMessage(xLogPos, nextStandbyMessageDeadline)
			if err != nil {
				app.Logger.Error("error in replication message processing", "error", err)
				continue
			}
		}
	}
}

// processPushObjects processes safe objects from the ObjectQueue and applies them to PostgreSQL.
func (p *Postgresql) processQueue(index int64) (int64, error) {

	objects := app.ObjectQueue.GetSafeObjectsFromIndex(index, p.systemInfo.Name)

	for _, object := range objects {
		objectsToPush := applyPushMixer(object, p.systemInfo.PushMixer)
		for _, pushObject := range objectsToPush {
			foundDuplicate := app.DuplicateChecker.CheckIfSeen(pushObject)
			if !foundDuplicate {
				switch pushObject.Operation {
				case "upsert":
					err := p.upsertObject(pushObject)
					if err != nil {
						return index, fmt.Errorf("error upserting JSON to PostgreSQL: %v", err)
					}
				case "delete":
					err := p.deleteObject(pushObject)
					if err != nil {
						return index, fmt.Errorf("error deleting from PostgreSQL: %v", err)
					}
				}

				app.DuplicateChecker.AddObject(object)
			}
		}
	}

	if len(objects) > 0 {
		index += int64(len(objects))
		app.ObjectQueue.SetSafeIndex(p.systemInfo.Name, index)
	}

	return index, nil
}

func (p *Postgresql) processReplicationMessage(clientXLogPos pglogrepl.LSN, nextStandbyMessageDeadline time.Time) (pglogrepl.LSN, time.Time, error) {
	// Send a standby status update to the replication connection if needed
	if time.Now().After(nextStandbyMessageDeadline) {
		err := pglogrepl.SendStandbyStatusUpdate(context.Background(), p.replConn, pglogrepl.StandbyStatusUpdate{WALWritePosition: clientXLogPos})
		if err != nil {
			return clientXLogPos, nextStandbyMessageDeadline, fmt.Errorf("SendStandbyStatusUpdate failed: %v", err)
		}
		nextStandbyMessageDeadline = time.Now().Add(time.Second * 10)
	}

	ctx, cancel := context.WithDeadline(context.Background(), nextStandbyMessageDeadline)
	rawMsg, err := p.replConn.ReceiveMessage(ctx)
	cancel()
	if err != nil {
		if pgconn.Timeout(err) {
			return clientXLogPos, nextStandbyMessageDeadline, nil // Not fatal, just continue
		}
		return clientXLogPos, nextStandbyMessageDeadline, fmt.Errorf("ReceiveMessage failed: %v", err)
	}

	if errMsg, ok := rawMsg.(*pgproto3.ErrorResponse); ok {
		return clientXLogPos, nextStandbyMessageDeadline, fmt.Errorf("received Postgres WAL error: %+v", errMsg)
	}

	msg, ok := rawMsg.(*pgproto3.CopyData)
	if !ok {
		log.Printf("Received unexpected message: %T\n", rawMsg)
		return clientXLogPos, nextStandbyMessageDeadline, nil // Not fatal, just continue
	}

	switch msg.Data[0] {
	case pglogrepl.PrimaryKeepaliveMessageByteID:
		pkm, err := pglogrepl.ParsePrimaryKeepaliveMessage(msg.Data[1:])
		if err != nil {
			return clientXLogPos, nextStandbyMessageDeadline, fmt.Errorf("ParsePrimaryKeepaliveMessage failed: %v", err)
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
			return clientXLogPos, nextStandbyMessageDeadline, fmt.Errorf("ParseXLogData failed: %v", err)
		}
		err = p.handleCdcEvent(&xld)
		if err != nil {
			return clientXLogPos, nextStandbyMessageDeadline, fmt.Errorf("error handling CDC event: %v", err)
		}
		if xld.WALStart > clientXLogPos {
			clientXLogPos = xld.WALStart
		}
	default:
		// Ignore other message types
	}
	return clientXLogPos, nextStandbyMessageDeadline, nil
}

type OldKeys struct {
	ColumnNames  []string `json:"keynames"`
	ColumnValues []any    `json:"keyvalues,omitempty"` // Optional, if not provided, the old keys are not included
}

type CdcChange struct {
	Kind         string   `json:"kind"`
	Schema       string   `json:"schema"`
	Table        string   `json:"table"`
	ColumnNames  []string `json:"columnnames"`
	ColumnTypes  []string `json:"columntypes"`
	ColumnValues []any    `json:"columnvalues"`
	OldKeys      *OldKeys `json:"oldkeys,omitempty"`
}

type CdcEvent struct {
	Change []*CdcChange `json:"change"`
}

func (p *Postgresql) handleCdcEvent(xld *pglogrepl.XLogData) error {

	var event CdcEvent
	err := json.Unmarshal(xld.WALData, &event)
	if err != nil {
		return fmt.Errorf("error unmarshalling CDC event: %v", err)
	}

	if app.Config.LogLevel == "debug" {
		app.AddToDebugStore(app.DebugMessage{Payload: event, Operation: "Received CDC event", System: p.systemInfo.Name})
	}

	for _, change := range event.Change {

		incomingObjects, err := p.createObjectsromCDCEvent(change)
		if err != nil {
			return fmt.Errorf("error creating objects from CDC event: %v", err)
		}

		for schemaName, incomingObject := range incomingObjects {

			schema, inMap := app.SchemaMap[schemaName]
			if !inMap {
				return fmt.Errorf("no schema found for object: %s", incomingObject.Schema)
			}

			err = schema.Validator.Validate(incomingObject)
			if err != nil {
				return fmt.Errorf("object failed validation for schema: %s, error: %v", schemaName, err)
			}

			foundDuplicate := app.DuplicateChecker.CheckIfSeen(incomingObject)
			if !foundDuplicate {
				app.ObjectQueue.AddSafeObject(incomingObject, p.systemInfo.Name)
			}
		}
	}

	return nil
}

func (p *Postgresql) upsertObject(object *app.Object) error {

	for locationInSystem := range (*p.systemInfo.PushMixer)[object.Schema] {

		presentSearchKeys := []string{}
		for _, field := range (*p.systemInfo.PushMixer)[object.Schema][locationInSystem].SearchKeys {
			_, ok := object.Payload[field]
			if ok {
				presentSearchKeys = append(presentSearchKeys, field)
			}
		}

		columnValues := []string{}
		for field := range object.Payload {
			columnValues = append(columnValues, fmt.Sprintf("%v = %v", field, object.Payload[field]))
		}

		whereClause := []string{}
		for _, field := range presentSearchKeys {
			whereClause = append(whereClause, fmt.Sprintf("%v = %v", field, object.Payload[field]))
		}

		query := fmt.Sprintf(`update %v set %v where %v;`, locationInSystem, strings.Join(columnValues, ", "), strings.Join(whereClause, " OR "))

		if app.Config.LogLevel == "debug" {
			app.AddToDebugStore(app.DebugMessage{Payload: object.Payload, Operation: fmt.Sprintf("Upserting into %v", locationInSystem), System: p.systemInfo.Name})
		}

		_, err := p.db.Exec(query)
		if err != nil {
			return fmt.Errorf("error executing upsert query: %v", err)
		}
	}

	return nil
}

// deleteFromPostgresql deletes a row from PostgreSQL based on the searchFields and payload.
func (p *Postgresql) deleteObject(object *app.Object) error {

	for locationInSystem := range (*p.systemInfo.PushMixer)[object.Schema] {

		presentSearchKeys := []string{}
		for _, field := range (*p.systemInfo.PushMixer)[object.Schema][locationInSystem].SearchKeys {
			_, ok := object.Payload[field]
			if ok {
				presentSearchKeys = append(presentSearchKeys, field)
			}
		}

		whereClause := []string{}
		for _, field := range presentSearchKeys {
			whereClause = append(whereClause, fmt.Sprintf("%v = %v", field, object.Payload[field]))
		}

		query := fmt.Sprintf(`delete from %v where %v;`, locationInSystem, strings.Join(whereClause, " OR "))

		if app.Config.LogLevel == "debug" {
			app.AddToDebugStore(app.DebugMessage{Payload: object.Payload, Operation: fmt.Sprintf("Deleting from %v", locationInSystem), System: p.systemInfo.Name})
		}

		_, err := p.db.Exec(query)
		if err != nil {
			return fmt.Errorf("error executing delete query: %v", err)
		}
	}

	return nil
}

func (p *Postgresql) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	app.Logger.Error("PostgreSQL does not support webhooks", "system", p.systemInfo.Name)
}

func (p *Postgresql) createObjectsromCDCEvent(change *CdcChange) (map[string]*app.Object, error) {

	incomingObject := &app.Object{
		Payload: map[string]any{},
	}

	switch change.Kind {
	case "insert", "update":
		incomingObject.Operation = "upsert"
		for i, colName := range change.ColumnNames {
			if change.ColumnValues[i] != nil {
				incomingObject.Payload[colName] = change.ColumnValues[i]
			}
		}
	case "delete":
		incomingObject.Operation = "delete"
		for i, colName := range change.OldKeys.ColumnNames {
			if change.OldKeys.ColumnValues != nil {
				incomingObject.Payload[colName] = change.OldKeys.ColumnValues[i]
			}
		}
	default:
		return nil, fmt.Errorf("unknown change kind: %s", change.Kind)
	}

	pullLocation := change.Schema + "." + change.Table

	incomingObjects := applyReceiveMixer(incomingObject, p.systemInfo.ReceiveMixer, pullLocation)

	if app.Config.LogLevel == "debug" {
		app.AddToDebugStore(app.DebugMessage{Payload: incomingObjects, Operation: "Created incoming objects from CDC event", System: p.systemInfo.Name})
	}

	return incomingObjects, nil
}

func validatePostgreSQLSystemInfo(systemInfo *SystemInfo) error {
	if len(*systemInfo.PushMixer) == 0 && len(*systemInfo.ReceiveMixer) == 0 {
		return fmt.Errorf("systemInfo must have at least one of PushMixer or ReceiveMixer configured")
	}
	return nil
}
