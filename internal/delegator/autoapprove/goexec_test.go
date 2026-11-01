package autoapprove

import "testing"

// TestGoProgramFlagsAsk proves #2736: under the built-in read-only set, a go
// command whose flags run a named program or write persistent state asks.
// Every spelling cmd/go accepts is a row (measured on go1.26: one or two
// dashes, =value or the next word; no abbreviation). Before #2736 only
// -vettool and -vettool=x were refused, so every other row here approved.
func TestGoProgramFlagsAsk(t *testing.T) {
	rules := parseAutoApproveRules(CommonReadonlyRules)
	for _, cmd := range []string{
		// -toolexec runs the program for every compile/cgo step.
		`go list -toolexec=/tmp/x -export ./...`,
		`go list --toolexec=/tmp/x -export ./...`,
		`go list -toolexec /tmp/x -export ./...`,
		`go list --toolexec /tmp/x -export ./...`,
		`go list -export ./... -toolexec=/tmp/x`,
		`go vet -toolexec=/tmp/x ./...`,
		`go vet --toolexec /tmp/x ./...`,
		`go list '-toolexec=/tmp/x' -export ./...`,
		`go list "--toolexec" /tmp/x -export ./...`,
		// -vettool: go vet scans the whole line for it, both dash forms.
		`go vet -vettool=/tmp/x ./...`,
		`go vet --vettool=/tmp/x ./...`,
		`go vet -vettool /tmp/x ./...`,
		`go vet --vettool /tmp/x ./...`,
		`go vet ./... --vettool=/tmp/x`,
		// The other program-naming flags.
		`go vet -exec=/tmp/x ./...`,
		`go list --exec /tmp/x ./...`,
		`go vet -fixtool=/tmp/x ./...`,
		`go list -compiler=gccgo -export ./...`,
		`go vet -compiler gccgo ./...`,
		`go list -ldflags=-extld=/tmp/x -export ./...`,
		`go vet --ldflags '-extld=/tmp/x' ./...`,
		// go env -w / -u write the go env file a later go command reads.
		`go env -w CC=/tmp/x`,
		`go env -w GOFLAGS=-toolexec=/tmp/x`,
		`go env --w CC=/tmp/x`,
		`go env -w=true CC=/tmp/x`,
		`go env --w=1 CC=/tmp/x`,
		`go env -u GOFLAGS`,
		`go env --u GOFLAGS`,
		`go env -u=true GOFLAGS`,
		`go env GOFLAGS -w`,
		// go vet -fix edits source files; go doc -http starts a server.
		`go vet -fix ./...`,
		`go vet --fix=true ./...`,
		`go doc -http`,
		`go doc --http fmt`,
		// Compound positions.
		`ls && go env -w CC=/tmp/x`,
		`echo $(go list -toolexec=/tmp/x -export .)`,
	} {
		assertApproval(t, rules, cmd, false)
	}
}

// TestGoReadOnlyFormsStayApproved is the other half of #2736: the common
// read-only go commands keep approval, including flags whose NAME a refusal
// uses on another subcommand (`go doc -u` shows unexported symbols; it is
// not `go env -u`) and flags that merely start like a refused one.
func TestGoReadOnlyFormsStayApproved(t *testing.T) {
	rules := parseAutoApproveRules(CommonReadonlyRules)
	for _, cmd := range []string{
		`go env`,
		`go env GOPATH GOCACHE`,
		`go env -json`,
		`go env -changed`,
		`go version`,
		`go version -m /usr/bin/go`,
		`go doc fmt.Println`,
		`go doc -u -all fmt`,
		`go doc -src strings.Cut`,
		`go help build`,
		`go list ./...`,
		`go list -f {{.Dir}} ./...`,
		`go list -json -deps ./...`,
		`go list -m all`,
		`go list -export -f {{.Export}} .`,
		`go list -tags integration ./...`,
		`go vet ./...`,
		`go vet -json ./...`,
		`go vet -diff ./...`,
		`go vet -tags linux ./internal/...`,
	} {
		assertApproval(t, rules, cmd, true)
	}
}

// TestGoProgramFlagsAskUnderBroadRule proves the refusal belongs to the
// command, not to the read-only set: under a user rule `Bash:go *` (which also
// matches the -C forms the built-in patterns never match) the program and
// state-writing flags still ask, while -C <dir> is stepped over to find the
// subcommand, so `go -C env doc -u` is doc's harmless -u.
func TestGoProgramFlagsAskUnderBroadRule(t *testing.T) {
	rules := parseAutoApproveRules([]string{"Bash:go *"})
	for _, c := range []struct {
		cmd  string
		want bool
	}{
		{`go -C /tmp/m list ./...`, true},
		{`go -C env doc -u fmt`, true},
		{`go -C=/tmp/m vet ./...`, true},
		{`go -C /tmp/m vet -toolexec=/tmp/x ./...`, false},
		{`go -C=/tmp/m list --toolexec /tmp/x ./...`, false},
		{`go -C /tmp/m env -w CC=/tmp/x`, false},
		{`go -C env env -u GOFLAGS`, false},
		{`go build -toolexec=/tmp/x ./...`, false},
		{`go test -exec /tmp/x ./...`, false},
		{`go run -exec=/tmp/x .`, false},
		{`go build ./...`, true},
	} {
		assertApproval(t, rules, c.cmd, c.want)
	}
	// A path-qualified go is judged by its base name.
	byPath := parseAutoApproveRules([]string{"Bash:/usr/bin/go *"})
	assertApproval(t, byPath, `/usr/bin/go list ./...`, true)
	assertApproval(t, byPath, `/usr/bin/go list -toolexec=/tmp/x -export ./...`, false)
}

// TestGoFlagName pins the token reading goRunsProgram rests on.
func TestGoFlagName(t *testing.T) {
	for _, tt := range []struct {
		tok    string
		name   string
		isFlag bool
	}{
		{"-toolexec", "toolexec", true},
		{"--toolexec", "toolexec", true},
		{"-toolexec=x", "toolexec", true},
		{"--toolexec=-x=y", "toolexec", true},
		{"-w", "w", true},
		{"-w=false", "w", true},
		{"--", "", false},
		{"-", "", false},
		{"---x", "", false},
		{"-=x", "", false},
		{"./...", "", false},
		{"env", "", false},
	} {
		name, isFlag := goFlagName(tt.tok)
		if name != tt.name || isFlag != tt.isFlag {
			t.Errorf("goFlagName(%q) = (%q, %v), want (%q, %v)", tt.tok, name, isFlag, tt.name, tt.isFlag)
		}
	}
}
