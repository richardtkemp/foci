package main

import (
	"foci/internal/session"
	"foci/internal/tools"
)

// chatFollowUpParent answers, for send_to_session's #3159 exception, which
// chat a caller may send its one follow-up to: the parent of a reflection-pass
// branch (interval, pre-compaction or session-end, by the branch_type its
// branch file records), when that parent is a session reflection runs on
// (session.SessionType.IsReflectable: chats, facets, independent sessions;
// spawns and crons are out of scope, Dick 2026-11-20 Q140). Any other caller
// gets ok=false and stays under the #1409 bar.
func chatFollowUpParent(sessions *session.Store, idx *session.SessionIndex) tools.ChatFollowUpParentFn {
	return func(callerKey string) (string, bool) {
		if sessions == nil || idx == nil {
			return "", false
		}
		meta, err := sessions.GetBranchMeta(callerKey)
		if err != nil || meta == nil || meta.ParentKey == "" || !session.IsChatFollowUpBranchType(meta.BranchType) {
			return "", false
		}
		parent, err := idx.Get(meta.ParentKey)
		if err != nil || !parent.SessionType.IsReflectable() {
			return "", false
		}
		return meta.ParentKey, true
	}
}
