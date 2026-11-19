package delegator

// AccountSpec is what a backend says about one of its logins (#3186). The
// gateway builds the login's state around it (internal/account).
type AccountSpec struct {
	// Key identifies the login among every backend's logins. Equal keys are
	// one account; the usage hold and the usage history are stored under it.
	Key string
	// ID is a short stable form of Key for names that must stay short (a
	// tmux session, a state file). "" for the host's login.
	ID string
	// Host marks the login of an agent that names no folder of its own.
	Host bool
	// Name names the login's location for a person, until its email is
	// known. "" = the login has no name of its own (a backend whose agents
	// all share one login).
	Name string
	// Dir is the folder where the backend keeps this login's files (Claude
	// Code: its config folder). "" = none.
	Dir string
	// Env is the login's part of the launch env of a session on it: what the
	// backend process must be given, on top of the agent's
	// backend_config.env, to run on this login. nil for the host's login.
	Env map[string]string
	// LoginEnv is what the pane of an interactive login is told
	// (relogin.Config.Env).
	LoginEnv []string
}

// AccountOf is the login of spec's backend whose folder is folder (""  = the
// host's login): Spec.Account, or, for a backend with one login for all its
// agents, the login named after the backend.
func AccountOf(spec Spec, folder string) (AccountSpec, error) {
	if spec.Account == nil {
		return AccountSpec{Key: spec.Name, Host: true}, nil
	}
	return spec.Account(folder)
}

// AccountAcceptor is implemented by a backend whose instances differ by
// login (Claude Code: the config folder its file operations read, and the
// env its process starts with). The manager calls SetAccount on a new
// instance, before any other use, with the account of the session the
// instance is for.
type AccountAcceptor interface {
	SetAccount(a AccountSpec)
}
