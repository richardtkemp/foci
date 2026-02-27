package agent

import (
	"regexp"

	"foci/config"
	"foci/log"
)

// CompiledPromptRule is a pre-compiled regex rule for prompt transformation.
type CompiledPromptRule struct {
	re      *regexp.Regexp
	replace string
}

// CompilePromptRules compiles the configured prompt rules into ready-to-use regexes.
// Invalid patterns are logged and skipped.
func CompilePromptRules(rules []config.PromptRule) []CompiledPromptRule {
	var compiled []CompiledPromptRule
	for _, r := range rules {
		re, err := regexp.Compile(r.Find)
		if err != nil {
			log.Errorf("prompt_rules", "invalid regex %q: %v", r.Find, err)
			continue
		}
		compiled = append(compiled, CompiledPromptRule{re: re, replace: r.Replace})
	}
	return compiled
}

// ApplyPromptRules runs all compiled rules in order on the message.
// Each rule's output becomes the next rule's input.
func ApplyPromptRules(rules []CompiledPromptRule, message string) string {
	for _, r := range rules {
		message = r.re.ReplaceAllString(message, r.replace)
	}
	return message
}
