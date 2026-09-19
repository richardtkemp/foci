package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
)

func scoreUsage() {
	fmt.Fprintf(os.Stderr, `Usage: foci score [-a agent] [-s session] [--turn id] [--obs id] [--user who] <name> <value> [comment...]

Record a human score on a turn's trace in Langfuse. The value is validated
against the rubric of that name when one is loaded (foci evals list); an
unknown name is a free-text axis whose shape is inferred (number → numeric,
yes/no → boolean, anything else → a category).

Target: --turn names an api.db turn_id ("<session>@<nanos>"); otherwise the
session's last completed turn (default session of the agent unless -s).
--obs scopes the score to one observation (hex span id) of that trace.

Flags:
  -a, --agent <id>     Target agent (env: FOCI_AGENT)
  -s, --session <key>  Session key (env: FOCI_SESSION)
  --turn <turn_id>     Exact turn to score
  --obs <span_id>      Observation within the turn
  --user <who>         Who is grading (default: $USER); part of the score's id

Examples:
  foci score -a fabulo quality 4 "good, but hedged the deploy question"
  foci score --turn 'clutch/c123@1789811615564559571' honesty yes
`)
}

func cmdScore(base string, args []string) error {
	if wantsHelp(args) {
		scoreUsage()
		return nil
	}
	agent, args := parseAgentFlag(args)
	agent = envDefault(agent, "FOCI_AGENT")
	sess, args := parseSessionFlag(args)
	sess = envDefault(sess, "FOCI_SESSION")
	turn, args := parseFlagValue(args, "turn")
	obs, args := parseFlagValue(args, "obs")
	user, args := parseFlagValue(args, "user")
	if user == "" {
		user = os.Getenv("USER")
	}
	if len(args) < 2 {
		scoreUsage()
		return fmt.Errorf("name and value are required")
	}
	body, _ := json.Marshal(map[string]string{
		"agent": agent, "session": sess, "turn": turn, "observation": obs,
		"name": args[0], "value": args[1], "comment": strings.Join(args[2:], " "), "user": user,
	})
	resp, err := client.Post(base+"/score", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(out)))
	}
	var r struct {
		ScoreID string  `json:"score_id"`
		TraceID string  `json:"trace_id"`
		TurnID  string  `json:"turn_id"`
		Name    string  `json:"name"`
		Value   float64 `json:"value"`
		Label   string  `json:"label"`
		Rubric  bool    `json:"rubric"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		fmt.Println(strings.TrimSpace(string(out)))
		return nil
	}
	val := fmt.Sprintf("%g", r.Value)
	if r.Label != "" {
		val = r.Label
	}
	how := "free-text axis"
	if r.Rubric {
		how = "rubric"
	}
	fmt.Printf("scored %s=%s (%s) on turn %s\n  trace %s  score %s\n", r.Name, val, how, r.TurnID, r.TraceID, r.ScoreID)
	return nil
}

func evalsUsage() {
	fmt.Fprintf(os.Stderr, `Usage: foci evals list [-a agent]

List the loaded rubrics (scoring axes) and any rubric files that failed to
load. With -a, only the human-graded rubrics that apply to that agent's chat
sessions — what a score control would offer there.
`)
}

func cmdEvals(base string, args []string) error {
	if wantsHelp(args) || len(args) == 0 {
		evalsUsage()
		if len(args) == 0 {
			return fmt.Errorf("subcommand required")
		}
		return nil
	}
	switch args[0] {
	case "list":
		agent, _ := parseAgentFlag(args[1:])
		agent = envDefault(agent, "FOCI_AGENT")
		url := base + "/evals/rubrics"
		if agent != "" {
			url += "?agent=" + agent
		}
		resp, err := client.Get(url)
		if err != nil {
			return fmt.Errorf("request failed: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		out, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(out)))
		}
		var r struct {
			Dir     string `json:"dir"`
			Scoring bool   `json:"scoring_available"`
			Rubrics []struct {
				Name, Kind, Type, Shape, Description, ConfigID string
				Version                                        int
				Agents                                         []string
			} `json:"rubrics"`
			Errors map[string]string `json:"errors"`
		}
		if err := json.Unmarshal(out, &r); err != nil {
			return fmt.Errorf("bad response: %w", err)
		}
		fmt.Printf("rubrics dir: %s   scoring: %v\n", r.Dir, map[bool]string{true: "available", false: "UNAVAILABLE (tracing off)"}[r.Scoring])
		if len(r.Rubrics) == 0 {
			fmt.Println("(no rubrics loaded)")
		} else {
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "NAME\tVER\tKIND\tSHAPE\tAGENTS\tCONFIG\tDESCRIPTION")
			for _, rb := range r.Rubrics {
				agents := "*"
				if len(rb.Agents) > 0 {
					agents = strings.Join(rb.Agents, ",")
				}
				cfg := "-"
				if rb.ConfigID != "" {
					cfg = "mirrored"
				}
				_, _ = fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%s\n", rb.Name, rb.Version, rb.Kind, rb.Shape, agents, cfg, rb.Description)
			}
			_ = tw.Flush()
		}
		for p, e := range r.Errors {
			fmt.Fprintf(os.Stderr, "SKIPPED %s: %s\n", p, e)
		}
		return nil
	default:
		evalsUsage()
		return fmt.Errorf("unknown evals subcommand %q", args[0])
	}
}

// parseSessionFlag extracts -s/--session (both spaced and = forms), mirroring
// parseAgentFlag; send/branch parse it inline in parseSendFlags.
func parseSessionFlag(args []string) (session string, rest []string) {
	for i := 0; i < len(args); i++ {
		if (args[i] == "-s" || args[i] == "--session") && i+1 < len(args) {
			rest = append(rest, args[:i]...)
			rest = append(rest, args[i+2:]...)
			return args[i+1], rest
		}
		for _, prefix := range []string{"--session=", "-s="} {
			if strings.HasPrefix(args[i], prefix) {
				rest = append(rest, args[:i]...)
				rest = append(rest, args[i+1:]...)
				return args[i][len(prefix):], rest
			}
		}
	}
	return "", args
}
