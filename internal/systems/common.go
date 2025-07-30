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

	err := schema.Validator.Validate(object.Payload)
	if err != nil {
		app.Logger.Error("Object failed schema validation", "object type", objectName, "error", err)
		return err
	}

	// Check that object.Type is a supported schema
	if _, ok := app.SchemaMap[object.Schema]; !ok {
		app.Logger.Error("Unsupported object type", "type", object.Schema)
		return fmt.Errorf("unsupported object type: %s", object.Schema)
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
func applyReceiveMixer(object app.Object, receiveMixer ReceiveMixer) map[string]app.Object {
	newObjs := make(map[string]app.Object)

	for schemaName, fields := range receiveMixer[object.Schema] {
		newObj := map[string]any{}

		for keyInObj, field := range fields {
			if field.Hardcode != nil && !helpers.IsZeroValue(field.Hardcode) {
				newObj[field.Field] = field.Hardcode
			} else {
				newObj[field.Field] = helpers.GetNestedValue(object.Payload, keyInObj)
			}
		}

		newObjs[schemaName] = app.Object{
			Schema:    schemaName,
			Operation: object.Operation,
			Payload:   newObj,
		}
	}

	return newObjs
}

func applyPushMixer(object app.Object, pushMixer PushMixer) map[string]app.Object {

	newObjects := make(map[string]app.Object)
	searchFields := []any{}

	for _, fields := range pushMixer[object.Schema] {
		newObj := app.Object{
			Operation: object.Operation,
			Schema:    object.Schema,
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
	}

	return newObjects
}
