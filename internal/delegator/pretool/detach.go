package pretool

// DetachRules are the Bash deny rules for a shell command that detaches a
// process from itself: one ended by `&` (the background fact), and nohup,
// setsid --fork or disown. A depth leaf's backend refuses them (#2625), and so
// does a shell job's check (#3286). Their names start with prefix; reason
// gives each one's refusal from what it refuses.
func DetachRules(prefix string, reason func(what string) string) []Rule {
	yes := true
	amp := Rule{Name: prefix + "bash_ampersand", Tool: bashTool, Action: ActionDeny,
		Reason: reason("a command ended by &"), Background: &yes}
	detach := Rule{Name: prefix + "bash_detach", Tool: bashTool, Action: ActionDeny,
		Reason: reason("nohup, setsid --fork or disown")}
	detach.Command = Patterns{
		`(\S*/)?(nohup|disown)(\s|$)`,
		`(\S*/)?setsid\s+(\S+\s+)*(--fork|-[A-Za-z]*f[A-Za-z]*)(\s|$)`,
	}
	return []Rule{amp, detach}
}
