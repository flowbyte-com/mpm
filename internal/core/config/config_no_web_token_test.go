package config

import (
	"reflect"
	"testing"
)

// TestConfigHasNoWebToken ensures the web_token field is gone from the
// config schema. The field existed only to authenticate `mpm web`; after
// the HTTP server is stripped, no caller should be reading it.
//
// Note: a JSON-only check is insufficient because `omitempty` on the
// string field would hide it whenever the value is empty, even when the
// field is still declared. We use reflection on the struct type so the
// assertion holds regardless of the value being marshaled.
func TestConfigHasNoWebToken(t *testing.T) {
	if _, ok := reflect.TypeOf(Config{}).FieldByName("WebToken"); ok {
		t.Fatalf("Config struct still has WebToken field")
	}
}
