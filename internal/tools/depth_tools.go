package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Tools returns the four depth-mode tools (#2588): depth_fanout, depth_report,
// depth_claims and depth_status, exported to delegated agents as the
// foci_depth_* shell functions. Each is gated live on [depth] enabled (#2603):
// while it is off the tool is hidden from the model's listings (Available) and
// refuses a call, which still reaches it through a running CC session's shell
// functions, written once at that session's start.
func (m *DepthManager) Tools() []*Tool {
	ts := []*Tool{m.fanoutTool(), m.reportTool(), m.claimsTool(), m.statusTool()}
	for _, t := range ts {
		m.gate(t)
	}
	return ts
}

// gate makes t follow the live [depth] enabled switch.
func (m *DepthManager) gate(t *Tool) {
	exec := t.Execute
	t.Available = m.Enabled
	t.Execute = func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
		if !m.Enabled() {
			return ToolResult{}, errDepthOff(t.Name)
		}
		return exec(ctx, params)
	}
}

func errDepthOff(tool string) error {
	return fmt.Errorf("%s: depth mode is off ([depth] enabled = false); it works again once it is turned back on", tool)
}

func (m *DepthManager) fanoutTool() *Tool {
	return &Tool{
		Name:       "depth_fanout",
		ExecExport: true,
		Description: "Depth mode, for multithreaded interactive work: whenever a user's request opens several discussion threads, use this to fan the conversation out into leaves, ONE point per leaf (never a list of questions), which the user works through one at a time inside this chat. " +
			"Each leaf is a fork of your context. To cover several questions, give each its own leaf. " +
			"You then end your turn and get ONE [DEPTH RESULTS] message when every leaf has reported. Only from an app conversation (or from a leaf). " +
			"Each leaf costs a copy of your current context, so prefer fanning out while the context is small.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"leaves": {
					"type": "array",
					"description": "The leaves, as a JSON array: [{\"title\": \"short point name\", \"prompt\": \"the ONE point this leaf discusses\", \"orientation\": \"optional: report format / focus for this leaf\"}, ...].",
					"items": {
						"type": "object",
						"properties": {
							"title": {"type": "string"},
							"prompt": {"type": "string"},
							"orientation": {"type": "string"}
						},
						"required": ["title", "prompt"]
					}
				},
				"orientation": {
					"type": "string",
					"description": "Default report format / focus for every leaf that does not set its own."
				}
			},
			"required": ["leaves"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			p, err := UnmarshalParams[struct {
				Leaves      json.RawMessage `json:"leaves"`
				Orientation string          `json:"orientation"`
			}](params)
			if err != nil {
				return ToolResult{}, err
			}
			leaves, err := parseDepthLeaves(p.Leaves)
			if err != nil {
				return ToolResult{}, err
			}
			out, err := m.fanout(ctx, SessionKeyFromContext(ctx), leaves, p.Orientation)
			if err != nil {
				return ToolResult{}, err
			}
			return TextResult(out), nil
		},
	}
}

// parseDepthLeaves accepts the leaves as a JSON array, or as a string holding
// one (a shell caller's --leaves value).
func parseDepthLeaves(raw json.RawMessage) ([]depthLeafSpec, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, fmt.Errorf("depth_fanout: leaves is required")
	}
	var leaves []depthLeafSpec
	if err := json.Unmarshal(raw, &leaves); err == nil {
		return leaves, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if err := json.Unmarshal([]byte(s), &leaves); err == nil {
			return leaves, nil
		}
	}
	return nil, fmt.Errorf(`depth_fanout: leaves must be a JSON array like [{"title": "...", "prompt": "..."}]`)
}

func (m *DepthManager) reportTool() *Tool {
	return &Tool{
		Name:        "depth_report",
		ExecExport:  true,
		Positional:  []string{"text"},
		Description: "Depth mode: report your point back to your parent, ONCE, in the shape your orientation asks for. After this the thread is outside the tree.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"text": {"type": "string", "description": "Your report (free text)."}
			},
			"required": ["text"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			p, err := UnmarshalParams[struct {
				Text string `json:"text"`
			}](params)
			if err != nil {
				return ToolResult{}, err
			}
			out, err := m.report(SessionKeyFromContext(ctx), p.Text)
			if err != nil {
				return ToolResult{}, err
			}
			return TextResult(out), nil
		},
	}
}

func (m *DepthManager) claimsTool() *Tool {
	return &Tool{
		Name:        "depth_claims",
		ExecExport:  true,
		Positional:  []string{"text"},
		Description: "Depth mode: the tree's claims board. 'list' shows what every node has claimed; 'add <text>' claims a sub-point and shows existing claims that look related (informational, never refused).",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"action": {"type": "string", "enum": ["list", "add"], "description": "list (default) or add."},
				"text": {"type": "string", "description": "For add: what you claim. A bare 'list' or 'add <text>' also works."}
			}
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			p, err := UnmarshalParams[struct {
				Action string `json:"action"`
				Text   string `json:"text"`
			}](params)
			if err != nil {
				return ToolResult{}, err
			}
			action, text := p.Action, strings.TrimSpace(p.Text)
			if action == "" {
				// foci_depth_claims list | foci_depth_claims add some text
				first, rest, _ := strings.Cut(text, " ")
				switch first {
				case "list", "":
					action, text = "list", ""
				case "add":
					action, text = "add", strings.TrimSpace(rest)
				default:
					action = "add"
				}
			}
			out, err := m.claims(SessionKeyFromContext(ctx), action, text)
			if err != nil {
				return ToolResult{}, err
			}
			return TextResult(out), nil
		},
	}
}

func (m *DepthManager) statusTool() *Tool {
	return &Tool{
		Name:        "depth_status",
		ExecExport:  true,
		Description: "Depth mode: the tree you are in, as text: each node's title, level, state, spend so far, and the report of the done ones.",
		Parameters:  json.RawMessage(`{"type": "object", "properties": {}}`),
		Execute: func(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
			return TextResult(m.status(SessionKeyFromContext(ctx))), nil
		},
	}
}
