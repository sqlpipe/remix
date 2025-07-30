package app

import "github.com/santhosh-tekuri/jsonschema/v6"

type Schema struct {
	Title      string
	SearchKeys []string
	Validator  *jsonschema.Schema
}

var SchemaMap = make(map[string]Schema)
