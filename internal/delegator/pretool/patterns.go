package pretool

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// Patterns is a list of regexes, any one of which may match. Config accepts
// either a single string (the original #2028 syntax) or an array of strings,
// so an alternation can be written as separate patterns instead of one
// regex.
type Patterns []string

// UnmarshalTOML implements toml.Unmarshaler.
func (p *Patterns) UnmarshalTOML(v any) error {
	switch x := v.(type) {
	case string:
		*p = Patterns{x}
		return nil
	case []any:
		out := make(Patterns, 0, len(x))
		for i, e := range x {
			s, ok := e.(string)
			if !ok {
				return fmt.Errorf("pattern %d: want a string, got %T", i, e)
			}
			out = append(out, s)
		}
		*p = out
		return nil
	}
	return fmt.Errorf("want a regex string or an array of them, got %T", v)
}

// UnmarshalJSON accepts a string as well as an array, matching the TOML form.
func (p *Patterns) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*p = Patterns{s}
		return nil
	}
	var list []string
	if err := json.Unmarshal(b, &list); err != nil {
		return err
	}
	*p = list
	return nil
}

func (p Patterns) validate() error {
	for _, pat := range p {
		if _, err := regexp.Compile(pat); err != nil {
			return err
		}
	}
	return nil
}

// compile returns the compiled patterns, each wrapped by wrap (a format with
// one %s, or "" for as-is). ok is false if any pattern fails to compile; the
// caller then treats the constraint as unmatched.
func (p Patterns) compile(wrap string) (res []*regexp.Regexp, ok bool) {
	for _, pat := range p {
		if wrap != "" {
			pat = fmt.Sprintf(wrap, pat)
		}
		re, err := regexp.Compile(pat)
		if err != nil {
			return nil, false
		}
		res = append(res, re)
	}
	return res, true
}

// anyMatch reports whether any pattern matches any of the subjects.
func anyMatch(res []*regexp.Regexp, subjects ...string) bool {
	for _, re := range res {
		for _, s := range subjects {
			if re.MatchString(s) {
				return true
			}
		}
	}
	return false
}
