package command

import (
	"errors"
	"fmt"
	"strings"

	"foci/internal/config"
	"foci/internal/configapprove"
)

// config_http.go keeps every HTTP route from writing foci.toml without the
// user's approval (#479). An agent reaches the command registry from its
// shell (`foci command "/config set k=v"`, or `foci send "/config ..."`),
// and those dispatches arrive with Source == SourceHTTP. There, a /config
// set or unset with a full value does not write: it turns into the same
// approval request as `foci config set` (configapprove), for the same key
// path, and answers at once with the request id. Wizards that write the file
// (/config set without a value, /agents new, /android) are refused over
// HTTP: they must be started from chat. Chat and app dispatches are not
// changed.

// ConfigApprovalFn submits one config change for the user's approval on
// behalf of sessionKey; value is TOML value text (unused for an unset).
type ConfigApprovalFn func(sessionKey string, op configapprove.Op, path, value string) configapprove.Status

// approvalRequested is the error an HTTP-mode writer returns instead of
// writing: it carries the approval request's status to the reply.
type approvalRequested struct{ st configapprove.Status }

func (e *approvalRequested) Error() string { return "config change sent for approval" }

// fromHTTP reports whether a dispatch arrived over the gateway's HTTP API.
func fromHTTP(req Request) bool { return req.Source == SourceHTTP }

// setTargetPath is the key path a registry write target addresses: the same
// path `foci config set` takes for the same key.
func setTargetPath(t config.SetTarget) config.KeyPath {
	if t.Section == "agents" {
		return config.KeyPath{AgentID: t.AgentID, Keys: strings.Split(t.Key, ".")}
	}
	return config.KeyPath{Keys: append(strings.Split(t.Section, "."), strings.Split(t.Key, ".")...)}
}

// httpConfigDeps returns a copy of deps whose writers request approval
// instead of writing, and which can start no wizard.
func httpConfigDeps(deps *ConfigSetDeps, sessionKey string) *ConfigSetDeps {
	d := *deps
	d.Registry = nil
	request := deps.RequestApprovalFn
	d.SetInFileFn = func(_ string, target config.SetTarget, value string) (string, error) {
		if request == nil {
			return "", errors.New("config approval is not available")
		}
		return "", &approvalRequested{request(sessionKey, configapprove.OpSet, setTargetPath(target).String(), value)}
	}
	d.UnsetInFileFn = func(_ string, target config.SetTarget) (string, error) {
		if request == nil {
			return "", errors.New("config approval is not available")
		}
		return "", &approvalRequested{request(sessionKey, configapprove.OpUnset, setTargetPath(target).String(), "")}
	}
	return &d
}

// isDirectSetArgs reports whether /config set args carry a full value (the
// forms that write without a wizard): <agent> <key>=<value>, section.key=value,
// or section key value.
func isDirectSetArgs(args string) bool {
	if _, _, ok := splitAgentSetArgs(args); ok {
		return true
	}
	return strings.Contains(args, "=") || len(strings.Fields(args)) == 3
}

// httpWizardRefusal is the reply to a file-writing wizard started over HTTP.
func httpWizardRefusal(what string) string {
	return fmt.Sprintf("%s writes foci.toml and must be started from chat, not over the HTTP API. To ask the user to approve one config change, run: foci config set <path> <value> (see the foci-config skill).", what)
}

// approvalReply renders an approval request's status as the command reply.
func approvalReply(st configapprove.Status) string {
	if st.Outcome == configapprove.Failed {
		return "Config change refused, nothing written: " + st.Reason
	}
	warning := ""
	if st.Warning != "" {
		warning = " " + st.Warning
	}
	return fmt.Sprintf("Config change sent to the user for approval (request %s): %s, current %s, new %s.%s Nothing changes unless they approve. Follow it with: foci config wait %s",
		st.ID, st.Path, st.Old, st.New, warning, st.ID)
}

// configWriteReply turns an HTTP-mode writer's approvalRequested error into
// the reply; any other result passes through.
func configWriteReply(text string, err error) (Response, error) {
	var ar *approvalRequested
	if errors.As(err, &ar) {
		return Response{Text: approvalReply(ar.st)}, nil
	}
	return Response{Text: text}, err
}
