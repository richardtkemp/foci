package main

import (
	"fmt"
	"os"

	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/tempdir"
)

var skillGitReportLog = log.NewComponentLogger("skill-git-report")

// sendSkillGitReport delivers a git-attributed skill-change report (commit
// message + diff of the touched files, formatted as markdown by
// skills.AttributeToGit) to the chat owning sessionKey, as a system-styled
// notice carrying the report as a document attachment.
//
// Two delivery paths, mirroring the existing compaction-summary flow
// (agent_platforms.go's CompactionNotifyFunc/CompactionDebugFunc — the
// established "system notice + attached detail" pattern in this codebase):
//   - App (implements both SessionNotifier and DetailAttacher): send a
//     system-styled notification, then attach the markdown as a blob-backed
//     detail on it — the client renders a tappable "view" chit. Same
//     mechanism compaction summaries use.
//   - Chat platforms (Telegram/Discord) or any AttachDetail failure: send a
//     system/injected text notice (Connection.SendInjectedMessage — the
//     "role: system" primitive every transport implements), then the
//     markdown as a real document attachment via SendDocument (mirrors
//     CompactionDebugFunc's temp-file + SendDocument pattern).
//
// No-op if no connection is resolved for the session/agent.
func sendSkillGitReport(connMgr platform.ConnectionManager, agentID, sessionKey, skillName, markdown string) {
	conn := connMgr.ForSessionOrPrimary(sessionKey, agentID)
	if conn == nil {
		return
	}
	notice := fmt.Sprintf("Skill updated: %s (git commit landed during reflection)", skillName)

	if sn, ok := conn.(platform.SessionNotifier); ok {
		if da, ok := conn.(platform.DetailAttacher); ok {
			if msgID := sn.SendNotificationToSession(sessionKey, notice); msgID != "" {
				if err := da.AttachDetail(msgID, notice, markdown); err == nil {
					return
				}
				skillGitReportLog.Debugf("attach detail failed for session=%s msgID=%s, falling back to document", sessionKey, msgID)
			}
		}
	}

	f, err := tempdir.Create("skill-update-*.md")
	if err != nil {
		skillGitReportLog.Warnf("create temp file: %v", err)
		return
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.WriteString(markdown); err != nil {
		_ = f.Close()
		skillGitReportLog.Warnf("write temp file: %v", err)
		return
	}
	_ = f.Close()

	if err := conn.SendInjectedMessage(sessionKey, notice); err != nil {
		skillGitReportLog.Debugf("injected notice failed for session=%s: %v", sessionKey, err)
	}
	if err := conn.SendDocument(f.Name(), ""); err != nil {
		skillGitReportLog.Warnf("send document for session=%s: %v", sessionKey, err)
	}
}
