package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"foci/internal/fap"
	"foci/internal/log"
)

var (
	askLog             = log.NewComponentLogger("ask")
	execbridgeLog      = log.NewComponentLogger("execbridge")
	http_requestLog    = log.NewComponentLogger("http_request")
	remindLog          = log.NewComponentLogger("remind")
	send_to_sessionLog = log.NewComponentLogger("send_to_session")
	spawnLog           = log.NewComponentLogger("spawn")
	summaryLog         = log.NewComponentLogger("summary")
	web_fetchLog       = log.NewComponentLogger("web_fetch")
	web_searchLog      = log.NewComponentLogger("web_search")
)

// AppInvoker routes a tool call to a connected app device and awaits its
// result. Implemented by *app.appConn (via the platform.Connection it
// satisfies); resolved at tool call time so a missing device returns
// gracefully rather than crashing registration.
//
// Returns fap.ToolResult directly (not a mirror struct) so *appConn — whose
// InvokeTool returns fap.ToolResult — actually satisfies this interface. An
// earlier local mirror (AppToolResult) had the same fields but a distinct type,
// so the runtime conn.(AppInvoker) assertion in tool_table.go always failed and
// the tool reported "no device" even when one was connected. fap is a leaf
// package (already imported by ask.go), so depending on it here is cycle-free.
type AppInvoker interface {
	InvokeTool(ctx context.Context, tool, action string, args json.RawMessage) (fap.ToolResult, error)
}

// AppToolProber is the optional capability probe an AppInvoker may also
// satisfy: whether any live device can run a device-side tool. The client tool
// uses it to tell "the phone's app is too old for this action" apart from "no
// phone at all" (#2802). *app.appConn implements it.
type AppToolProber interface {
	CanRunTool(tool string) bool
}

// ErrNoCapableDevice is returned by an AppInvoker when no live device can run
// the requested device tool: none is connected, or only ones without that
// handler (a desktop app). app.ErrNoLiveDevice is this value.
var ErrNoCapableDevice = errors.New("app: no connected device can run this tool")

// ErrNoAppDevice is the narrower ErrNoCapableDevice where no app socket is live
// at all. It wraps ErrNoCapableDevice. app.ErrNoConnectedDevice is this value.
var ErrNoAppDevice = fmt.Errorf("app: no app device is connected: %w", ErrNoCapableDevice)

// appToolInvokeTimeout bounds how long the server waits for a device tool.result
// (including across "pending" keepalives). It MUST exceed the device's own hard
// cap for tracking a task, so the device always sends a terminal result before
// the server gives up — otherwise a slow result is lost. Device cap is ~55s
// (see AndroidTaskerToolHandler); 60s here leaves margin for round-trips.
const appToolInvokeTimeout = 60 * time.Second

// Device-side tool names on the FAP wire (tool.invoke's `tool` field, and the
// `tool:<name>` hello capability). clientDeviceTool is the handler of the
// current app; legacyTaskerDeviceTool is the Tasker-only handler of apps built
// before #2802, which the server still drives for the two Tasker actions.
const (
	clientDeviceTool       = "client"
	legacyTaskerDeviceTool = "android"
)

// clientParams is the agent-facing parameter set of the client tool. Every
// action reads the subset it needs; Args holds the shell's bare words, which
// clientActionSpec.positional maps onto the named fields.
type clientParams struct {
	Action   string          `json:"action"`
	Args     []string        `json:"args"`
	Time     string          `json:"time"`
	Label    string          `json:"label"`
	Days     string          `json:"days"`
	Duration string          `json:"duration"`
	Number   string          `json:"number"`
	Name     string          `json:"name"`
	Text     *string         `json:"text"`
	URL      string          `json:"url"`
	Seconds  json.RawMessage `json:"seconds"`
	Task     string          `json:"task"`
	Par1     string          `json:"par1"`
	Par2     string          `json:"par2"`
	Type     string          `json:"type"`
	From     string          `json:"from"`
	To       string          `json:"to"`
	Limit    json.RawMessage `json:"limit"`
}

// clientActionSpec describes one action: the named params its bare words fill
// (in order; the last one takes all remaining words, space-joined, when join
// is set), the usage shown in the description, and the builder that checks
// the params and returns the JSON args for the device.
type clientActionSpec struct {
	name       string
	usage      string
	positional []string
	join       bool
	build      func(p *clientParams) (map[string]any, error)
}

// Ring duration bounds for ring_phone: a default when the agent gives none,
// and a cap so a typo cannot ring the phone for an hour.
const (
	ringDefaultSeconds = 30
	ringMaxSeconds     = 300
	// timerMaxSeconds is Android's AlarmClock.EXTRA_LENGTH range (1..86400).
	timerMaxSeconds = 24 * 60 * 60
)

// health action defaults. The window and the record cap keep one call from
// pouring weeks of heart-rate samples into the agent's context.
const (
	healthDefaultDays  = 7
	healthDefaultLimit = 100
	healthMaxLimit     = 1000
)

// healthTypes is every Health Connect data type the app can read (Dick's ruling
// "Health:all!"). Keep in step with HealthDataType in foci-client.
var healthTypes = []string{
	"active_calories_burned", "basal_body_temperature", "basal_metabolic_rate", "blood_glucose",
	"blood_pressure", "body_fat", "body_temperature", "body_water_mass", "bone_mass", "cervical_mucus",
	"cycling_pedaling_cadence", "distance", "elevation_gained", "exercise", "floors_climbed", "heart_rate",
	"heart_rate_variability", "height", "hydration", "intermenstrual_bleeding", "lean_body_mass",
	"menstruation_flow", "menstruation_period", "mindfulness", "nutrition", "ovulation_test",
	"oxygen_saturation", "planned_exercise", "power", "respiratory_rate", "resting_heart_rate",
	"sexual_activity", "skin_temperature", "sleep", "speed", "steps", "steps_cadence",
	"total_calories_burned", "vo2_max", "weight", "wheelchair_pushes",
	"medical_allergies_intolerances", "medical_conditions", "medical_laboratory_results",
	"medical_medications", "medical_personal_details", "medical_practitioner_details",
	"medical_pregnancy", "medical_procedures", "medical_social_history", "medical_vaccines",
	"medical_visits", "medical_vital_signs",
}

var clientActions = []clientActionSpec{
	{name: "tasker_list", usage: "tasker_list — the Tasker tasks the user allowlisted in the app",
		build: func(p *clientParams) (map[string]any, error) {
			return map[string]any{"task": "", "par1": "", "par2": ""}, nil
		}},
	{name: "tasker_perform", usage: "tasker_perform <task> [par1] [par2] — run an allowlisted Tasker task (%par1/%par2; JSON-stringify structured args into par1)",
		positional: []string{"task", "par1", "par2"},
		build: func(p *clientParams) (map[string]any, error) {
			if strings.TrimSpace(p.Task) == "" {
				return nil, errors.New("task: empty; give the Tasker task name (see tasker_list)")
			}
			return map[string]any{"task": p.Task, "par1": p.Par1, "par2": p.Par2}, nil
		}},
	{name: "set_alarm", usage: `set_alarm <HH:MM> [label] [--days mon,wed] — an alarm in the Clock app at the next HH:MM (24 h, phone local time), repeating on the days given; no date`,
		positional: []string{"time", "label"}, join: true,
		build: func(p *clientParams) (map[string]any, error) {
			h, m, err := parseClockTime(p.Time)
			if err != nil {
				return nil, err
			}
			days, err := parseWeekdays(p.Days)
			if err != nil {
				return nil, err
			}
			return map[string]any{"hour": h, "minute": m, "label": p.Label, "days": days}, nil
		}},
	{name: "set_timer", usage: `set_timer <duration> [label] — a countdown timer in the Clock app (duration: 90, 90s, 2m, 1h30m; at most 24h)`,
		positional: []string{"duration", "label"}, join: true,
		build: func(p *clientParams) (map[string]any, error) {
			secs, err := parseTimerDuration(p.Duration)
			if err != nil {
				return nil, err
			}
			return map[string]any{"seconds": secs, "label": p.Label}, nil
		}},
	{name: "call", usage: "call <number> — place a phone call at once (default off in the app)",
		positional: []string{"number"}, join: true,
		build: func(p *clientParams) (map[string]any, error) {
			n, err := checkPhoneNumber(p.Number)
			if err != nil {
				return nil, err
			}
			return map[string]any{"number": n}, nil
		}},
	{name: "contact_lookup", usage: "contact_lookup <name> — contacts whose name contains it, with phone numbers and emails (default off in the app)",
		positional: []string{"name"}, join: true,
		build: func(p *clientParams) (map[string]any, error) {
			n := strings.TrimSpace(p.Name)
			if n == "" {
				return nil, errors.New("name: empty; give a name or part of one")
			}
			return map[string]any{"name": n}, nil
		}},
	{name: "clipboard", usage: "clipboard <text> — put the text on the phone's clipboard",
		positional: []string{"text"}, join: true,
		build: func(p *clientParams) (map[string]any, error) {
			if p.Text == nil || *p.Text == "" {
				return nil, errors.New("text: empty; give the text to copy")
			}
			return map[string]any{"text": *p.Text}, nil
		}},
	{name: "open_url", usage: "open_url <url> — open an http, https or geo: URL in the app that handles it",
		positional: []string{"url"},
		build: func(p *clientParams) (map[string]any, error) {
			u, err := checkOpenURL(p.URL)
			if err != nil {
				return nil, err
			}
			return map[string]any{"url": u}, nil
		}},
	{name: "location", usage: "location — the phone's position: latitude, longitude, accuracy and fix time (default off in the app)",
		build: func(p *clientParams) (map[string]any, error) { return map[string]any{}, nil }},
	{name: "ring_phone", usage: fmt.Sprintf("ring_phone [seconds] — ring loudly, even on silent or vibrate (default %ds, at most %ds)", ringDefaultSeconds, ringMaxSeconds),
		positional: []string{"seconds"},
		build: func(p *clientParams) (map[string]any, error) {
			secs, err := parseRingSeconds(p.Seconds)
			if err != nil {
				return nil, err
			}
			return map[string]any{"seconds": secs}, nil
		}},
	{name: "read_notifications", usage: "read_notifications — the notifications in the phone's shade right now: app, title, text, time. No history (default off in the app)",
		build: func(p *clientParams) (map[string]any, error) { return map[string]any{}, nil }},
	{name: "health", usage: fmt.Sprintf("health [type] [--from WHEN] [--to WHEN] [--limit N] — Health Connect data, newest first: steps, sleep, heart_rate, weight, exercise, nutrition and every other type; no type lists all %d and which are granted. WHEN is YYYY-MM-DD (phone local, --to inclusive) or RFC 3339; default the last %d days, %d records, at most %d (default off in the app)", len(healthTypes), healthDefaultDays, healthDefaultLimit, healthMaxLimit),
		positional: []string{"type"},
		build:      buildHealthArgs},
}

func clientActionByName(name string) (clientActionSpec, bool) {
	for _, a := range clientActions {
		if a.name == name {
			return a, true
		}
	}
	return clientActionSpec{}, false
}

func clientActionNames() []string {
	names := make([]string, len(clientActions))
	for i, a := range clientActions {
		names[i] = a.name
	}
	return names
}

var clockTimeRe = regexp.MustCompile(`^(\d{1,2}):(\d{2})$`)

// parseClockTime checks an HH:MM 24-hour clock time (00:00-23:59).
func parseClockTime(s string) (int, int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, errors.New("time: empty; give HH:MM (24 h), e.g. 07:30")
	}
	m := clockTimeRe.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, fmt.Errorf("time: %q is not HH:MM (24 h, 00:00-23:59)", s)
	}
	h, _ := strconv.Atoi(m[1])
	mi, _ := strconv.Atoi(m[2])
	if h > 23 || mi > 59 {
		return 0, 0, fmt.Errorf("time: %q is outside 00:00-23:59", s)
	}
	return h, mi, nil
}

var weekdayOrder = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

var weekdayAliases = map[string]string{
	"mon": "mon", "monday": "mon",
	"tue": "tue", "tues": "tue", "tuesday": "tue",
	"wed": "wed", "wednesday": "wed",
	"thu": "thu", "thur": "thu", "thurs": "thu", "thursday": "thu",
	"fri": "fri", "friday": "fri",
	"sat": "sat", "saturday": "sat",
	"sun": "sun", "sunday": "sun",
}

// parseWeekdays reads a comma- or space-separated weekday list into the
// canonical three-letter names, Monday first, without duplicates. Empty means
// a one-off alarm.
func parseWeekdays(s string) ([]string, error) {
	seen := map[string]bool{}
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		d, ok := weekdayAliases[strings.ToLower(f)]
		if !ok {
			return nil, fmt.Errorf("days: unknown weekday %q (use mon,tue,wed,thu,fri,sat,sun)", f)
		}
		seen[d] = true
	}
	days := []string{}
	for _, d := range weekdayOrder {
		if seen[d] {
			days = append(days, d)
		}
	}
	return days, nil
}

// parseTimerDuration reads whole seconds ("90") or a Go duration ("90s",
// "2m", "1h30m") into a positive whole number of seconds, at most 24h.
func parseTimerDuration(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("duration: empty; give e.g. 90, 90s, 2m or 1h30m")
	}
	var secs float64
	if n, err := strconv.Atoi(s); err == nil {
		secs = float64(n)
	} else if d, err := time.ParseDuration(s); err == nil {
		secs = d.Seconds()
	} else {
		return 0, fmt.Errorf("duration: %q is not a duration (e.g. 90, 90s, 2m, 1h30m)", s)
	}
	if secs < 1 {
		return 0, fmt.Errorf("duration: %q must be at least 1 second", s)
	}
	if secs > timerMaxSeconds {
		return 0, fmt.Errorf("duration: %q is longer than 24h", s)
	}
	return int(secs), nil
}

var phoneNumberRe = regexp.MustCompile(`^\+?[0-9*#() .\-]+$`)

// checkPhoneNumber accepts a dialable number: digits with an optional leading
// +, and the separators people write (spaces, dots, dashes, brackets), plus *
// and # for service codes.
func checkPhoneNumber(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("number: empty; give the phone number to call")
	}
	if !phoneNumberRe.MatchString(s) || !strings.ContainsAny(s, "0123456789") {
		return "", fmt.Errorf("number: %q is not a phone number", s)
	}
	return s, nil
}

// checkOpenURL accepts http(s) URLs with a host and geo: URIs; any other
// scheme (intent:, file:, content:, javascript:, ...) is refused.
func checkOpenURL(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("url: empty; give an http, https or geo: URL")
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("url: %q does not parse: %v", s, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		if u.Host == "" {
			return "", fmt.Errorf("url: %q has no host", s)
		}
	case "geo":
		if u.Opaque == "" && u.RawQuery == "" {
			return "", fmt.Errorf("url: %q has no position (e.g. geo:51.46,-0.11)", s)
		}
	default:
		return "", fmt.Errorf("url: scheme %q is not allowed (only http, https and geo)", u.Scheme)
	}
	return s, nil
}

// parseRingSeconds reads ring_phone's optional duration: absent → default;
// a JSON number or a numeric string (the shell's bare word) otherwise.
func parseRingSeconds(raw json.RawMessage) (int, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return ringDefaultSeconds, nil
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err != nil {
		var s string
		if err2 := json.Unmarshal(raw, &s); err2 != nil {
			return 0, fmt.Errorf("seconds: %s is not a number", raw)
		}
		s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "s"))
		if s == "" {
			return ringDefaultSeconds, nil
		}
		v, err3 := strconv.ParseFloat(s, 64)
		if err3 != nil {
			return 0, fmt.Errorf("seconds: %q is not a number", s)
		}
		n = v
	}
	if n < 1 || n != float64(int(n)) {
		return 0, fmt.Errorf("seconds: %v must be a whole number of at least 1", n)
	}
	if n > ringMaxSeconds {
		return 0, fmt.Errorf("seconds: %v is more than the %d-second cap", n, ringMaxSeconds)
	}
	return int(n), nil
}

// buildHealthArgs checks the health action's arguments. No type asks for the
// list of data types, which takes no other argument.
func buildHealthArgs(p *clientParams) (map[string]any, error) {
	t := strings.TrimSpace(p.Type)
	if t == "" {
		if p.From != "" || p.To != "" || (len(p.Limit) > 0 && string(p.Limit) != "null") {
			return nil, errors.New("type: empty; --from, --to and --limit need a data type")
		}
		return map[string]any{}, nil
	}
	known := false
	for _, h := range healthTypes {
		known = known || h == t
	}
	if !known {
		return nil, fmt.Errorf("type: unknown data type %q (known: %s)", t, strings.Join(healthTypes, ", "))
	}
	args := map[string]any{"type": t}
	from, fromDate, err := parseHealthWhen("from", p.From)
	if err != nil {
		return nil, err
	}
	to, toDate, err := parseHealthWhen("to", p.To)
	if err != nil {
		return nil, err
	}
	if !from.IsZero() && !to.IsZero() && fromDate == toDate {
		// Two dates may be equal (--to is inclusive: that one day); two instants may not.
		if to.Before(from) || (!toDate && to.Equal(from)) {
			return nil, fmt.Errorf("to: %q is not after from %q", p.To, p.From)
		}
	}
	if !from.IsZero() {
		args["from"] = strings.TrimSpace(p.From)
	}
	if !to.IsZero() {
		args["to"] = strings.TrimSpace(p.To)
	}
	limit, err := parseHealthLimit(p.Limit)
	if err != nil {
		return nil, err
	}
	args["limit"] = limit
	return args, nil
}

// parseHealthWhen reads a window bound: YYYY-MM-DD (the phone applies its own
// time zone) or RFC 3339. Empty returns the zero time. The bool says it was a date.
func parseHealthWhen(name, s string) (time.Time, bool, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false, nil
	}
	if d, err := time.Parse(time.DateOnly, s); err == nil {
		return d, true, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, false, nil
	}
	return time.Time{}, false, fmt.Errorf("%s: %q is neither YYYY-MM-DD nor RFC 3339", name, s)
}

// parseHealthLimit reads the optional record cap: a JSON number or a numeric
// string (the shell's flag value).
func parseHealthLimit(raw json.RawMessage) (int, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return healthDefaultLimit, nil
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err != nil {
		var s string
		if err2 := json.Unmarshal(raw, &s); err2 != nil {
			return 0, fmt.Errorf("limit: %s is not a number", raw)
		}
		v, err3 := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err3 != nil {
			return 0, fmt.Errorf("limit: %q is not a number", s)
		}
		n = v
	}
	if n < 1 || n != float64(int(n)) {
		return 0, fmt.Errorf("limit: %v must be a whole number of at least 1", n)
	}
	if n > healthMaxLimit {
		return 0, fmt.Errorf("limit: %v is more than the cap of %d records", n, healthMaxLimit)
	}
	return int(n), nil
}

// fillPositional maps the shell's bare words onto the action's named params,
// skipping params already given by name. The last param takes every remaining
// word (space-joined) when the action joins; otherwise extra words are an error.
func fillPositional(spec clientActionSpec, p *clientParams) error {
	words := p.Args
	if len(words) == 0 {
		return nil
	}
	slots := map[string]func(string){
		"task":     func(v string) { p.Task = v },
		"par1":     func(v string) { p.Par1 = v },
		"par2":     func(v string) { p.Par2 = v },
		"time":     func(v string) { p.Time = v },
		"label":    func(v string) { p.Label = v },
		"duration": func(v string) { p.Duration = v },
		"number":   func(v string) { p.Number = v },
		"name":     func(v string) { p.Name = v },
		"text":     func(v string) { p.Text = &v },
		"url":      func(v string) { p.URL = v },
		"seconds":  func(v string) { p.Seconds, _ = json.Marshal(v) },
		"type":     func(v string) { p.Type = v },
	}
	isSet := map[string]bool{
		"task": p.Task != "", "par1": p.Par1 != "", "par2": p.Par2 != "",
		"time": p.Time != "", "label": p.Label != "", "duration": p.Duration != "",
		"number": p.Number != "", "name": p.Name != "", "text": p.Text != nil,
		"url": p.URL != "", "seconds": len(p.Seconds) > 0 && string(p.Seconds) != "null",
		"type": p.Type != "",
	}
	var free []string
	for _, s := range spec.positional {
		if !isSet[s] {
			free = append(free, s)
		}
	}
	for i, slot := range free {
		if len(words) == 0 {
			break
		}
		if i == len(free)-1 && spec.join {
			slots[slot](strings.Join(words, " "))
			words = nil
			break
		}
		slots[slot](words[0])
		words = words[1:]
	}
	if len(words) > 0 {
		return fmt.Errorf("unexpected extra argument %q for %s (usage: %s)", words[0], spec.name, spec.usage)
	}
	return nil
}

func clientDescription() string {
	var b strings.Builder
	b.WriteString(`Act on the user's Android phone through the foci app. Give an action and its arguments.
Actions:`)
	for _, a := range clientActions {
		b.WriteString("\n  ")
		b.WriteString(a.usage)
	}
	b.WriteString(`
The user switches each action on or off in the app (Settings > Advanced > Phone actions) and grants there the Android permission it needs; a switched-off or ungranted action is refused with an error saying so. Calls, contact lookup, location, read_notifications and health start switched off. Nothing prompts the user at call time.
Returns the phone's result (what was done, or the data asked for), or an error string: no phone connected, a phone that cannot run the action (desktop app, or a phone app that needs an update), or a phone that did not answer in time.`)
	return b.String()
}

// NewClientTool creates the `client` tool (shell: foci_client) — the
// agent-facing half of the device-tool mechanism, and the successor of
// app_android (#2802). It checks the action's arguments, then sends an FAP
// `tool.invoke` to the newest connected phone and awaits the reply.
//
// A phone running an app from before #2802 advertises only the Tasker handler
// ("android"): the two Tasker actions still reach it there as list/perform, and
// every other action is refused with an update-the-app error rather than sent.
//
// The invoker resolver is a func() so a missing app connection at process start
// doesn't disable the tool — it just returns "no device" when actually called,
// and the agent never sees the tool vanish between registrations.
func NewClientTool(invoker func() (AppInvoker, bool)) *Tool {
	return &Tool{
		Name:        "client",
		ExecExport:  true,
		Positional:  []string{"action"},
		RestParam:   "args",
		Description: clientDescription(),
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"action": {"type": "string", "enum": ` + mustJSON(clientActionNames()) + `, "description": "The phone action to run"},
				"args": {"type": "array", "items": {"type": "string"}, "description": "The action's arguments as bare words, in the order its usage shows (the shell form: foci_client set_alarm 07:30 \"Take T to nursery\"). Fills the named parameters not given otherwise."},
				"time": {"type": "string", "description": "set_alarm: HH:MM, 24 h, phone local time"},
				"label": {"type": "string", "description": "set_alarm / set_timer: the label shown on the alarm or timer"},
				"days": {"type": "string", "description": "set_alarm: repeat weekdays, comma-separated (mon,tue,wed,thu,fri,sat,sun); omit for a one-off alarm"},
				"duration": {"type": "string", "description": "set_timer: 90, 90s, 2m or 1h30m"},
				"number": {"type": "string", "description": "call: the phone number"},
				"name": {"type": "string", "description": "contact_lookup: a name or part of one"},
				"text": {"type": "string", "description": "clipboard: the text to copy"},
				"url": {"type": "string", "description": "open_url: an http, https or geo: URL"},
				"seconds": {"type": "integer", "description": "ring_phone: how long to ring (default 30, at most 300)"},
				"task": {"type": "string", "description": "tasker_perform: the Tasker task name, from tasker_list"},
				"par1": {"type": "string", "description": "tasker_perform: first param (%par1). For structured args, JSON-stringify into this slot."},
				"par2": {"type": "string", "description": "tasker_perform: second param (%par2)"},
				"type": {"type": "string", "description": "health: the data type, e.g. steps, sleep, heart_rate, weight; omit to list every type and whether it is granted"},
				"from": {"type": "string", "description": "health: start of the window, YYYY-MM-DD (phone local) or RFC 3339; default 7 days before to"},
				"to": {"type": "string", "description": "health: end of the window, YYYY-MM-DD (phone local, that whole day) or RFC 3339; default now"},
				"limit": {"type": "integer", "description": "health: most records to return, newest first (default 100, at most 1000)"}
			},
			"required": ["action"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			var p clientParams
			if err := json.Unmarshal(params, &p); err != nil {
				return TextResult(""), fmt.Errorf("client: parse params: %w", err)
			}
			spec, ok := clientActionByName(p.Action)
			if !ok {
				return ToolResult{}, fmt.Errorf("unknown action %q (actions: %s)", p.Action, strings.Join(clientActionNames(), ", "))
			}
			if err := fillPositional(spec, &p); err != nil {
				return ToolResult{}, err
			}
			args, err := spec.build(&p)
			if err != nil {
				return ToolResult{}, fmt.Errorf("%s: %w. Nothing was sent to the phone", spec.name, err)
			}
			argsJSON, _ := json.Marshal(args)

			inv, ok := invoker()
			if !ok {
				return ToolResult{}, errNoPhoneConnected
			}

			// Bound the wait even if the caller's ctx has no deadline. A device
			// "pending" keepalive doesn't end this wait — InvokeTool keeps
			// waiting for the terminal result up to appToolInvokeTimeout.
			ctx, cancel := context.WithTimeout(ctx, appToolInvokeTimeout)
			defer cancel()

			res, err := inv.InvokeTool(ctx, clientDeviceTool, spec.name, argsJSON)
			if errors.Is(err, ErrNoCapableDevice) && !errors.Is(err, ErrNoAppDevice) {
				res, err = invokeLegacy(ctx, inv, spec.name, argsJSON)
			}
			if err != nil {
				return ToolResult{}, clientInvokeError(spec.name, err)
			}
			if res.Status == fap.ToolStatusError && strings.HasPrefix(res.Error, phoneUnknownActionPrefix) {
				// An app older than this server's action list answers this way.
				return ToolResult{}, clientInvokeError(spec.name, errAppTooOld)
			}
			text, err := clientResultText(res)
			return TextResult(text), err
		},
	}
}

// phoneUnknownActionPrefix starts the error FociClientToolHandler returns for an
// action it does not know. The server checked the action, so the app is older
// than the server.
const phoneUnknownActionPrefix = "unknown action:"

var errNoPhoneConnected = errors.New("no Android device connected: the foci app is not open on any device. Ask the user to open the foci app on their phone")

// errAppTooOld marks a phone whose app predates the client handler and the
// action is not one its legacy Tasker handler can run.
var errAppTooOld = errors.New("phone app too old")

// invokeLegacy serves a phone whose app predates #2802: the Tasker actions go
// to its "android" handler as list/perform with today's args; any other action
// is refused (never sent) with an update-the-app error. No legacy phone either
// leaves ErrNoCapableDevice in place.
func invokeLegacy(ctx context.Context, inv AppInvoker, action string, args json.RawMessage) (fap.ToolResult, error) {
	legacyAction := map[string]string{"tasker_list": "list", "tasker_perform": "perform"}[action]
	if legacyAction == "" {
		if pr, ok := inv.(AppToolProber); ok && pr.CanRunTool(legacyTaskerDeviceTool) {
			return fap.ToolResult{}, errAppTooOld
		}
		return fap.ToolResult{}, ErrNoCapableDevice
	}
	return inv.InvokeTool(ctx, legacyTaskerDeviceTool, legacyAction, args)
}

func clientInvokeError(action string, err error) error {
	switch {
	case errors.Is(err, ErrNoAppDevice):
		return errNoPhoneConnected
	case errors.Is(err, errAppTooOld):
		return fmt.Errorf("the foci app on the phone is too old for %s. Ask the user to update the foci app on their phone. Nothing was done", action)
	case errors.Is(err, ErrNoCapableDevice):
		return errors.New("no connected device can run foci_client: no phone has the foci app open (a desktop app cannot run phone actions). Ask the user to open the foci app on their phone")
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("the phone did not answer within %s; %s may or may not have run", appToolInvokeTimeout, action)
	default:
		return err
	}
}

// clientResultText reads the phone's answer. Only a completed action is a
// result: anything else comes back as an error, so a script sees a non-zero exit.
func clientResultText(res fap.ToolResult) (string, error) {
	switch res.Status {
	case fap.ToolStatusCompleted:
		if len(res.Output) > 0 {
			return string(res.Output), nil
		}
		return "(task completed; no output)", nil
	case fap.ToolStatusPending:
		// Only reached if the task ran longer than appToolInvokeTimeout —
		// the device kept it running but the server gave up waiting.
		msg := fmt.Sprintf("Task is still running on the device and did not finish within %s; its result won't be delivered.", appToolInvokeTimeout)
		if res.Error != "" {
			msg += " " + res.Error
		}
		return "", errors.New(msg)
	case fap.ToolStatusError:
		return "", fmt.Errorf("device tool error: %s", res.Error)
	default:
		return "", fmt.Errorf("unknown status %q: %s", res.Status, res.Error)
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
