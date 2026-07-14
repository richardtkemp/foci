package agent

import (
	"regexp"

	"foci/internal/config"
)

// CompiledTransform is a pre-compiled regex rule for message transformation.
type CompiledTransform struct {
	re      *regexp.Regexp
	replace string
}

// CompileTransforms compiles the configured message transforms into ready-to-use regexes.
// Invalid patterns are logged and skipped.
func CompileTransforms(rules []config.MessageTransform) []CompiledTransform {
	var compiled []CompiledTransform
	for _, r := range rules {
		re, err := regexp.Compile(r.Find)
		if err != nil {
			message_transformsLog.Errorf("invalid regex %q: %v", r.Find, err)
			continue
		}
		compiled = append(compiled, CompiledTransform{re: re, replace: r.Replace})
	}
	return compiled
}

// ApplyTransforms runs all compiled transforms in order on the message.
// Each rule's output becomes the next rule's input.
func ApplyTransforms(rules []CompiledTransform, message string) string {
	for _, r := range rules {
		message = r.re.ReplaceAllString(message, r.replace)
	}
	return message
}
