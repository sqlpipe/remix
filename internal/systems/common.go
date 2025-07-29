package systems

import (
	"fmt"

	"github.com/sqlpipe/remix/internal/app"
	"github.com/sqlpipe/remix/internal/helpers"
)

// validateObject removes nil values from obj, validates it against the schema, and logs errors if any.
func validateObject(schemaName string, object app.Object, objectName string) error {
	schema, inMap := app.SchemaMap[schemaName]
	if !inMap {
		app.Logger.Error("No schema found for object", "object", objectName)
		return fmt.Errorf("no schema found for object: %s", objectName)
	}

	for k, v := range object.Payload {
		if v == nil {
			delete(object.Payload, k)
		}
	}

	err := schema.Validate(object.Payload)
	if err != nil {
		app.Logger.Error("Object failed schema validation", "object type", objectName, "error", err)
		return err
	}

	// Check that object.Type is a supported schema
	if _, ok := app.SchemaMap[object.Type]; !ok {
		app.Logger.Error("Unsupported object type", "type", object.Type)
		return fmt.Errorf("unsupported object type: %s", object.Type)
	}

	// Check that object.Operation is one of "upsert" or "delete"
	if object.Operation != "upsert" && object.Operation != "delete" {
		app.Logger.Error("Unsupported operation", "operation", object.Operation)
		return fmt.Errorf("unsupported operation: %s", object.Operation)
	}

	return nil
}

// applyReceiveMixer applies the ReceiveMixer to the given input and returns a map of schemaName to new model
// This implies that one input can create multiple objects to be put in the queue
func applyReceiveMixer(objectName string, operationType string, obj map[string]any, receiveMixer ReceiveMixer) map[string]app.Object {
	newObjs := make(map[string]app.Object)

	for schemaName, fields := range receiveMixer[objectName] {
		newObj := map[string]any{}

		for keyInObj, field := range fields {
			if field.Hardcode != nil && !helpers.IsZeroValue(field.Hardcode) {
				newObj[field.Field] = field.Hardcode
			} else {
				newObj[field.Field] = helpers.GetNestedValue(obj, keyInObj)
			}
		}

		newObjs[schemaName] = app.Object{
			Type:      schemaName,
			Operation: operationType,
			Payload:   newObj,
		}
	}

	return newObjs
}
