package config

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func TestNoSecretsInConfig(t *testing.T) {
	// Proves that no Config struct fields carry credential names (token, key,
	// password), enforcing the invariant that secrets stay out of the config struct
	// and are always resolved at runtime via the secrets store.
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
