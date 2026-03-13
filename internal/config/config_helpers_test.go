package config

import (
	"reflect"
	"strings"
	"testing"

	tomlParser "github.com/BurntSushi/toml"
)

// mockSecrets implements config.SecretGetter for testing.
type mockSecrets map[string]string

func (m mockSecrets) Get(key string) (string, bool) {
	v, ok := m[key]
	return v, ok
}

// collectTOMLKeys walks a struct type recursively and returns all leaf TOML key paths.
func collectTOMLKeys(t reflect.Type, prefix string) []string {
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() == reflect.Slice {
		t = t.Elem()
		if t.Kind() == reflect.Ptr {
			t = t.Elem()
		}
	}
	if t.Kind() != reflect.Struct {
		return nil
	}

	var keys []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("toml")
		if tag == "" || tag == "-" {
			continue
		}
		if idx := strings.Index(tag, ","); idx != -1 {
			tag = tag[:idx]
		}

		fullKey := tag
		if prefix != "" {
			fullKey = prefix + "." + tag
		}

		ft := f.Type
		if ft.Kind() == reflect.Ptr {
			ft = ft.Elem()
		}

		switch {
		case ft.Kind() == reflect.Map:
			// Dynamic keys — include the map itself but not contents
			keys = append(keys, fullKey)
		case ft.Kind() == reflect.Slice && ft.Elem().Kind() == reflect.Struct:
			// Slice of structs — recurse into element type
			keys = append(keys, collectTOMLKeys(ft.Elem(), fullKey)...)
		case ft.Kind() == reflect.Struct:
			if ft.Implements(reflect.TypeOf((*tomlParser.Unmarshaler)(nil)).Elem()) ||
				reflect.PointerTo(ft).Implements(reflect.TypeOf((*tomlParser.Unmarshaler)(nil)).Elem()) {
				// Custom unmarshaler (e.g. ToolCallDisplay) — leaf key
				keys = append(keys, fullKey)
			} else {
				keys = append(keys, collectTOMLKeys(ft, fullKey)...)
			}
		default:
			keys = append(keys, fullKey)
		}
	}
	return keys
}

func TestParseByteSize(t *testing.T) {
	// Proves that ParseByteSize correctly converts human-readable size strings
	// (with KB/MB/GB suffixes, case-insensitive) to byte counts, and returns
	// errors for empty or malformed input.
	tests := []struct {
		name    string
		input   string
		want    int
		wantErr bool
	}{
		{"plain number", "100", 100, false},
		{"kilobytes", "1KB", 1024, false},
		{"kilobytes lowercase", "1kb", 1024, false},
		{"megabytes", "1MB", 1024 * 1024, false},
		{"megabytes lowercase", "1mb", 1024 * 1024, false},
		{"gigabytes", "1GB", 1024 * 1024 * 1024, false},
		{"gigabytes lowercase", "1gb", 1024 * 1024 * 1024, false},
		{"with spaces", "  100  ", 100, false},
		{"64MB example", "64MB", 64 * 1024 * 1024, false},
		{"empty string", "", 0, true},
		{"invalid format", "abc", 0, true},
		{"zero bytes", "0", 0, true},
		{"negative bytes", "-10", 0, true},
		{"decimal kb not supported", "1.5KB", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseByteSize(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseByteSize(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if err == nil && got != tt.want {
				t.Errorf("ParseByteSize(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}
