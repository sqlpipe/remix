package systems

import (
	"fmt"

	"github.com/sqlpipe/remix/internal/app"
	"github.com/sqlpipe/remix/internal/helpers"
)

// validateObject removes nil values from obj, validates it against the schema, and logs errors if any.
func validateObject(object *app.Object) error {
	schema, inMap := app.SchemaMap[object.Schema]
	if !inMap {
		return fmt.Errorf("no schema found for object: %s", object.Schema)
	}

	for k, v := range object.Payload {
		if v == nil {
			delete(object.Payload, k)
		}
	}

	return schema.Validator.Validate(object.Payload)
}

func applyReceiveMixer(incomingObject *app.Object, receiveMixer *ReceiveMixer, pullLocationName string) map[string]*app.Object {

	canonicalObjects := make(map[string]*app.Object)

	for schemaName, schema := range (*receiveMixer)[pullLocationName] {

		canonicalObject := app.Object{
			Schema:    schemaName,
			Operation: incomingObject.Operation,
			Payload:   make(map[string]any),
		}

		for keyInObject, field := range schema {
			canonicalObject.Payload[field.Field] = helpers.GetNestedValue(incomingObject.Payload, keyInObject)
		}

		canonicalObjects[schemaName] = &canonicalObject
	}

	return canonicalObjects
}

func applyPushMixer(canonicalObject *app.Object, pushMixer *PushMixer) map[string]*app.Object {

	remixedObjects := make(map[string]*app.Object)

	for _, pushLocation := range (*pushMixer)[canonicalObject.Schema] {
		remixedObject := &app.Object{
			Operation: canonicalObject.Operation,
			Schema:    canonicalObject.Schema,
			Payload:   make(map[string]any),
		}

		for keyInSchema, field := range pushLocation.Fields {
			if _, ok := canonicalObject.Payload[keyInSchema]; ok {
				remixedObject.Payload[field.Field] = canonicalObject.Payload[keyInSchema]
			}
		}

		remixedObjects[canonicalObject.Schema] = remixedObject
	}

	return remixedObjects
}
