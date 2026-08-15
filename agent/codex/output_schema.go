package codex

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
)

type outputSchemaFile struct {
	SchemaPath string
	Cleanup    func() error
}

func createOutputSchemaFile(schema any) (outputSchemaFile, error) {
	if schema == nil {
		return outputSchemaFile{Cleanup: func() error { return nil }}, nil
	}

	if !isJSONObject(schema) {
		return outputSchemaFile{}, errors.New("outputSchema must be a JSON object")
	}

	dir, err := os.MkdirTemp("", "codex-output-schema-")
	if err != nil {
		return outputSchemaFile{}, err
	}
	schemaPath := filepath.Join(dir, "schema.json")
	cleanup := func() error {
		return os.RemoveAll(dir)
	}

	encoded, err := json.Marshal(schema)
	if err != nil {
		_ = cleanup()
		return outputSchemaFile{}, err
	}
	if err := os.WriteFile(schemaPath, encoded, 0o600); err != nil {
		_ = cleanup()
		return outputSchemaFile{}, err
	}
	return outputSchemaFile{SchemaPath: schemaPath, Cleanup: cleanup}, nil
}

func isJSONObject(value any) bool {
	if value == nil {
		return false
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Map, reflect.Struct:
		return true
	default:
		return false
	}
}
