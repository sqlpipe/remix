package helpers

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
)

type Envelope map[string]any

func WriteJSON(w http.ResponseWriter, status int, data Envelope, headers http.Header) error {
	js, err := json.MarshalIndent(data, "", "\t")
	if err != nil {
		return err
	}

	js = append(js, '\n')

	for key, value := range headers {
		w.Header()[key] = value
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(js)

	return nil
}

func GetNestedValue(obj map[string]any, dottedKey string) any {
	keys := strings.Split(dottedKey, ".")
	var val any = obj
	for _, k := range keys {
		m, ok := val.(map[string]any)
		if !ok {
			return nil
		}
		val, ok = m[k]
		if !ok {
			return nil
		}
	}
	return val
}

func IsZeroValue(x any) bool {
	if x == nil {
		return true
	}
	return reflect.ValueOf(x).IsZero()
}
