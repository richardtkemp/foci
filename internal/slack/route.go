package slack

import "foci/internal/config"

// router is the pure agent-selection table (#2459 req 2): which agent a
// conversation routes to. Config maps each Slack channel to exactly one agent
// (a duplicate mapping is a load-time config error, so the map is injective
// by construction); DMs go to one configured default agent, and no default
// means DMs start no turn.
type router struct {
	defaultAgent string
	channels     map[string]string
}

func newRouter(s *config.SlackSpecific) router {
	r := router{channels: make(map[string]string)}
	if s == nil {
		return r
	}
	r.defaultAgent = s.DefaultAgent
	for _, ch := range s.Channels {
		r.channels[ch.ID] = ch.Agent
	}
	return r
}

// forDM returns the agent that answers direct messages. ok=false when no
// default agent is configured: DMs start no turn (and get no reply).
func (r router) forDM() (agentID string, ok bool) {
	return r.defaultAgent, r.defaultAgent != ""
}

// forChannel returns the agent mapped to a channel. ok=false when the channel
// has no mapping: a mention there starts no turn; the bot replies once in the
// thread that it is not set up for the channel.
func (r router) forChannel(channelID string) (agentID string, ok bool) {
	agentID, ok = r.channels[channelID]
	return agentID, ok && agentID != ""
}

// names reports whether agentID is reachable through the router — the
// default agent or the agent of any mapped channel. SetupAgentConnection
// uses it to decide which agents get a Slack connection.
func (r router) names(agentID string) bool {
	if agentID == "" {
		return false
	}
	if agentID == r.defaultAgent {
		return true
	}
	for _, mapped := range r.channels {
		if mapped == agentID {
			return true
		}
	}
	return false
}
