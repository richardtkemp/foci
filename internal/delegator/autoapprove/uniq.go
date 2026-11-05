package autoapprove

import "strings"

// uniqCommandUnsafe refuses uniq's second operand (an output file). Recognize
// only known BSD/GNU options, before the input operand: treating unknown options
// or abbreviations as value-taking could accidentally hide an output filename.
// Optional values (-D, --all-repeated, --group) must be attached, never skipped
// as a separate word. Stop parsing options at the first operand, as BSD does.
func uniqCommandUnsafe(words []shellWord) bool {
	for _, w := range words[1:] {
		// Even input* can expand to TWO operands, without yielding any flag.
		if w.hasLiveGlob || w.hasLiveBrace || w.hasLiveSubst {
			return true
		}
	}
	options := true
	input := false
	for i := 1; i < len(words); i++ {
		tok := words[i].visible
		if options && tok == "--" {
			options = false
			continue
		}
		if options && strings.HasPrefix(tok, "--") {
			name, value, attached := strings.Cut(tok, "=")
			switch name {
			case "--count", "--repeated", "--ignore-case", "--unique", "--zero-terminated", "--help", "--version":
				if attached {
					return true
				}
			case "--all-repeated", "--group":
				// Both have optional, attached-only values. No following word is consumed.
			case "--skip-fields", "--skip-chars", "--check-chars":
				if !attached {
					i++
					if i == len(words) {
						return true
					}
					value = words[i].visible
				}
				if !uniqNumericValue(value) {
					return true
				}
			default:
				return true
			}
			continue
		}
		if options && len(tok) > 1 && tok[0] == '-' {
			for j := 1; j < len(tok); j++ {
				switch tok[j] {
				case 'c', 'd', 'i', 'u', 'z':
				case 'D':
					// Treat BSD's attached separator as part of this token. For
					// GNU clusters this is conservative: never hide a following
					// word as an option value.
					j = len(tok) - 1
				case 'f', 's', 'w':
					value := tok[j+1:]
					if value == "" {
						i++
						if i == len(words) {
							return true
						}
						value = words[i].visible
					}
					if !uniqNumericValue(value) {
						return true
					}
					j = len(tok) - 1
				default:
					return true
				}
			}
			continue
		}
		if input {
			return true
		}
		input = true
		options = false
	}
	return false
}

// uniqNumericValue accepts only explicit decimal option values, not another
// option or an operand that a different uniq implementation might not consume.
func uniqNumericValue(value string) bool {
	if value == "" {
		return false
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
