package main

import (
	"time"

	"foci/internal/askgw"
	"foci/internal/config"
	"foci/internal/platform"
	"foci/internal/question"
)

func setupAskgw(cfg *config.Config, agents map[string]*agentInstance, agentOrder []string, connMgr platform.ConnectionManager) *askgw.Server {
	if !cfg.Askgw.Enabled {
		return nil
	}

	resolveAgent := func(frameAgent string) string {
		if frameAgent != "" {
			return frameAgent
		}
		if cfg.Askgw.DefaultAgent != "" {
			return cfg.Askgw.DefaultAgent
		}
		if cfg.MasterAgent != "" {
			return cfg.MasterAgent
		}
		if len(agentOrder) > 0 {
			return agentOrder[0]
		}
		return ""
	}

	resolveSession := func(frameAgent string) (agentID, sessionKey string) {
		agentID = resolveAgent(frameAgent)
		if agentID == "" {
			return "", ""
		}
		inst := agents[agentID]
		if inst == nil {
			return agentID, ""
		}
		sk := defaultSessionKeyFor(inst.ag, agentID)
		return agentID, sk
	}

	present := func(agentID, sessionKey, msgID, text, summary string, choices []question.Choice, onResponse func(data string)) bool {
		resolve := connResolver(connMgr, sessionKey, agentID)
		conn := resolve()
		if conn == nil {
			return false
		}
		buttons := make([]platform.ButtonChoice, len(choices))
		for i, c := range choices {
			buttons[i] = platform.ButtonChoice{Label: c.Label, Data: c.Data}
		}
		_, err := platform.SendInteractiveMessageWithID(resolve, msgID, summary, text, buttons, func(choice platform.ButtonChoice) string {
			onResponse(choice.Data)
			if choice.Data == question.CancelData {
				return "❌ Cancelled"
			}
			return "✅ " + choice.Label
		}, func() {
			onResponse(question.CancelData)
		})
		return err == nil
	}

	cancelPrompt := func(msgID, finalText string) {
		if err := platform.CancelInteractiveMessage(msgID, finalText); err != nil {
			askgwLog.Warnf("cancel prompt %s: %v", msgID, err)
		}
	}

	timeout := time.Duration(cfg.Askgw.DefaultTimeoutSecs) * time.Second

	srv, err := askgw.NewServer(askgw.ServerDeps{
		SocketPath:     cfg.Askgw.SocketPath,
		AllowedUIDs:    cfg.Askgw.AllowedUIDs,
		MaxFrameBytes:  cfg.Askgw.MaxFrameBytes,
		DefaultTimeout: timeout,
		Group:          cfg.Askgw.ResolvedGroup(),
		Present:        present,
		CancelPrompt:   cancelPrompt,
		ResolveSession: resolveSession,
	})
	if err != nil {
		askgwLog.Errorf("config error: %v", err)
		return nil
	}
	if err := srv.Start(); err != nil {
		askgwLog.Errorf("failed to start: %v", err)
		return nil
	}
	return srv
}
