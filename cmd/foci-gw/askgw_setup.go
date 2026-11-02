package main

import (
	"time"

	"foci/internal/app"
	"foci/internal/askgw"
	"foci/internal/config"
	"foci/internal/platform"
	"foci/internal/question"
	"foci/internal/route"
)

func setupAskgw(cfg *config.Config, agents *agentRegistry, connMgr platform.ConnectionManager) *askgw.Server {
	if !cfg.Askgw.Enabled {
		return nil
	}

	// An id from the startup config (or a client that has not seen a live
	// rename) resolves through gwAgentRenames to the id it runs under (#2787).
	resolveAgent := func(frameAgent string) string {
		if frameAgent != "" {
			return gwAgentRenames.resolve(frameAgent)
		}
		if cfg.Askgw.DefaultAgent != "" {
			return gwAgentRenames.resolve(cfg.Askgw.DefaultAgent)
		}
		if cfg.MasterAgent != "" {
			return gwAgentRenames.resolve(cfg.MasterAgent)
		}
		if inst, ok := agents.Default(); ok {
			return inst.id
		}
		return ""
	}

	resolveSession := func(frameAgent string) (agentID, sessionKey string) {
		agentID = resolveAgent(frameAgent)
		if agentID == "" {
			return "", ""
		}
		inst, _ := agents.Get(agentID)
		if inst == nil {
			return agentID, ""
		}
		sk := defaultSessionKeyFor(inst.ag, agentID)
		return agentID, sk
	}

	chatPresent := askgwPresentFn(connMgr)

	present := askgwPresenter(app.PresentAskgwPrompt, chatPresent)

	cancelPrompt := func(msgID, finalText string) {
		if app.CloseAskgwPrompt(msgID) {
			return
		}
		if err := platform.CancelInteractiveMessage(msgID, finalText); err != nil {
			askgwLog.Warnf("cancel prompt %s: %v", msgID, err)
		}
	}

	// editMessage renders a `notify` frame onto the answered ask's own
	// message, in place — same session-aware edit preference the compaction
	// notify path uses (agent_platforms.go): SessionNotifier where the
	// transport supports per-session edits (Telegram/Discord), else a plain
	// ButtonSender edit (app, already session-bound via its connection).
	editMessage := func(agentID, sessionKey, msgID, text string) bool {
		conn := connMgr.ForSessionOrPrimary(sessionKey, agentID)
		if conn == nil {
			return false
		}
		if sn, ok := conn.(platform.SessionNotifier); ok {
			return sn.EditNotificationInSession(sessionKey, msgID, text) == nil
		}
		if bs, ok := conn.(platform.ButtonSender); ok {
			return bs.EditMessageText(msgID, text) == nil
		}
		return false
	}

	// notifyFallback posts a standalone message when editMessage can't
	// (platform msgID unknown, or the edit itself failed).
	notifyFallback := func(agentID, sessionKey, text string) {
		route.NotifySessionChat(connMgr, agentID, sessionKey, text)
	}

	timeout := time.Duration(cfg.Askgw.DefaultTimeoutSecs) * time.Second

	srv, err := askgw.NewServer(askgw.ServerDeps{
		SocketPath:     cfg.Askgw.SocketPath,
		AllowedUIDs:    cfg.Askgw.AllowedUIDs,
		MaxFrameBytes:  cfg.Askgw.MaxFrameBytes,
		DefaultTimeout: timeout,
		Group:          cfg.Askgw.ResolvedGroup(),
		Present:        present,
		EditMessage:    editMessage,
		NotifyFallback: notifyFallback,
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

// appPromptFn shows an askgw question app-wide (app.PresentAskgwPrompt);
// false means it was not shown. host is the ask's origin (#2800).
type appPromptFn func(id, header, text, host string, buttons []platform.ButtonChoice, onChoice func(data string)) bool

// askgwPresenter is askgw's PresentFn (#2707): the question is shown app-wide,
// as a dialog over whatever is on screen on every app device, not posted into
// an agent's chat. When appPresent refuses — no app provider, or no paired
// device with a build that can show it — it goes to the agent chat (chat), as
// before: a sudo approval must never be invisible. An app-wide prompt returns
// an empty platform msgID, so a later `notify` goes to notifyFallback: there
// is no chat message to edit.
func askgwPresenter(appPresent appPromptFn, chat askgw.PresentFn) askgw.PresentFn {
	return func(agentID, sessionKey, msgID, text, summary, origin string, choices []question.Choice, onResponse func(data string)) (string, askgw.PresentOutcome) {
		if appPresent(msgID, summary, text, origin, askgwButtons(choices), onResponse) {
			return "", askgw.Presented
		}
		return chat(agentID, sessionKey, msgID, text, summary, origin, choices, onResponse)
	}
}

// askgwButtons maps askgw's question choices onto platform buttons.
func askgwButtons(choices []question.Choice) []platform.ButtonChoice {
	buttons := make([]platform.ButtonChoice, len(choices))
	for i, c := range choices {
		buttons[i] = platform.ButtonChoice{Label: c.Label, Data: c.Data}
	}
	return buttons
}
