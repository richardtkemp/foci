package config

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func TestNoSecretsInConfig(t *testing.T) {
	// Config structs must not contain credential fields.
	// Secrets belong in secrets.toml, resolved via the secrets store at runtime.
	secretPatterns := []*regexp.Regexp{
		regexp.MustCompile(`_token$`),  // api_token, setup_token — but not max_output_tokens
		regexp.MustCompile(`_key$`),    // api_key, brave_api_key
		regexp.MustCompile(`password`), // password, password_hash
		regexp.MustCompile(`^key$`),    // bare "key"
		regexp.MustCompile(`^token$`),  // bare "token"
	}

	keys := collectTOMLKeys(reflect.TypeOf(Config{}), "")
	for _, key := range keys {
		parts := strings.Split(key, ".")
		leaf := strings.ToLower(parts[len(parts)-1])
		for _, pat := range secretPatterns {
			if pat.MatchString(leaf) {
				t.Errorf("config field %q matches %s — secrets belong in secrets.toml", key, pat)
			}
		}
	}
}
