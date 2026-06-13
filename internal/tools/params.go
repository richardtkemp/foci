package tools

import (
	"encoding/json"
	"fmt"
)

// UnmarshalParams parses a tool's JSON parameters into T, wrapping parse errors
// with a consistent message. It replaces the repeated tool-Execute boilerplate
//
//	var p T
//	if err := json.Unmarshal(input, &p); err != nil {
//		return ToolResult{}, fmt.Errorf("parse params: %w", err)
//	}
//
// with `p, err := UnmarshalParams[T](input)`.
func UnmarshalParams[T any](input json.RawMessage) (T, error) {
	var p T
	if err := json.Unmarshal(input, &p); err != nil {
		return p, fmt.Errorf("parse params: %w", err)
	}
	return p, nil
}
