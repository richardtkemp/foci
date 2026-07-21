package session

import "testing"

func TestSessionTypeForBranch(t *testing.T) {
	cases := map[string]SessionType{
		"facet":              SessionTypeFacet,
		"spawn":              SessionTypeSpawn,
		"reflection":         SessionTypeReflection,
		"consolidation":      SessionTypeReflection,
		"compaction-memory":  SessionTypeReflection,
		"session-end-memory": SessionTypeReflection,
		"keepalive":          SessionTypeKeepalive,
		"background":         SessionTypeBackgroundTask,
		"nudge-extraction":   SessionTypeBackgroundTask,
		"branch":             SessionTypeBackgroundTask,
		"":                   SessionTypeUnknown,
		"totally-unknown":    SessionTypeUnknown,
	}
	for bt, want := range cases {
		if got := SessionTypeForBranch(bt); got != want {
			t.Errorf("SessionTypeForBranch(%q) = %q, want %q", bt, got, want)
		}
	}
}

func TestSessionTypesCategorised(t *testing.T) {
	all := []SessionType{
		SessionTypeChat, SessionTypeFacet, SessionTypeIndependent,
		SessionTypeSpawn, SessionTypeReflection, SessionTypeKeepalive,
		SessionTypeBackgroundTask, SessionTypeUnknown,
	}
	reflectable := map[SessionType]bool{
		SessionTypeChat: true, SessionTypeFacet: true, SessionTypeIndependent: true,
	}
	for _, ty := range all {
		if ty.IsReflectable() != reflectable[ty] {
			t.Errorf("%s: IsReflectable()=%v, want %v", ty, ty.IsReflectable(), reflectable[ty])
		}
	}
	if got, want := reflectableTypesSQL(), "'chat', 'facet', 'independent'"; got != want {
		t.Errorf("reflectableTypesSQL() = %q, want %q", got, want)
	}
	// Every branch_type a creation site can pass must map into the taxonomy.
	allSet := map[SessionType]bool{}
	for _, ty := range all {
		allSet[ty] = true
	}
	for _, bt := range []string{"facet", "spawn", "reflection", "consolidation", "compaction-memory", "session-end-memory", "keepalive", "background", "nudge-extraction", "branch", ""} {
		if st := SessionTypeForBranch(bt); !allSet[st] {
			t.Errorf("SessionTypeForBranch(%q)=%s not in taxonomy", bt, st)
		}
	}
}

func TestIsUserFacingAndReflectable(t *testing.T) {
	userFacing := map[SessionType]bool{SessionTypeChat: true, SessionTypeFacet: true, SessionTypeIndependent: true}
	for _, ty := range []SessionType{SessionTypeChat, SessionTypeFacet, SessionTypeIndependent, SessionTypeSpawn, SessionTypeReflection, SessionTypeKeepalive, SessionTypeBackgroundTask, SessionTypeUnknown} {
		if ty.IsUserFacing() != userFacing[ty] {
			t.Errorf("%s.IsUserFacing()=%v want %v", ty, ty.IsUserFacing(), userFacing[ty])
		}
	}
}

func TestIsOneshot(t *testing.T) {
	// Reflection/keepalive/background-task/spawn are oneshot (a single
	// headless turn that terminates — see #1430); chat/facet/independent
	// never are (persistent, interactive). 'unknown' (legacy) is
	// deliberately NOT oneshot either — see IsOneshot's doc comment.
	oneshot := map[SessionType]bool{
		SessionTypeReflection:     true,
		SessionTypeKeepalive:      true,
		SessionTypeBackgroundTask: true,
		SessionTypeSpawn:          true,
	}
	for _, ty := range []SessionType{
		SessionTypeChat, SessionTypeFacet, SessionTypeIndependent,
		SessionTypeSpawn, SessionTypeReflection, SessionTypeKeepalive,
		SessionTypeBackgroundTask, SessionTypeUnknown,
	} {
		if ty.IsOneshot() != oneshot[ty] {
			t.Errorf("%s.IsOneshot()=%v want %v", ty, ty.IsOneshot(), oneshot[ty])
		}
	}
}

func TestIsBarredFromSessionSend(t *testing.T) {
	// #1409: only reflection/consolidation/keepalive are barred from
	// send_to_session outright — narrower than IsOneshot(), which also
	// covers spawn/background-task (those may still legitimately send; see
	// IsBarredFromSessionSend's doc comment).
	barred := map[SessionType]bool{
		SessionTypeReflection: true,
		SessionTypeKeepalive:  true,
	}
	for _, ty := range []SessionType{
		SessionTypeChat, SessionTypeFacet, SessionTypeIndependent,
		SessionTypeSpawn, SessionTypeReflection, SessionTypeKeepalive,
		SessionTypeBackgroundTask, SessionTypeUnknown,
	} {
		if ty.IsBarredFromSessionSend() != barred[ty] {
			t.Errorf("%s.IsBarredFromSessionSend()=%v want %v", ty, ty.IsBarredFromSessionSend(), barred[ty])
		}
	}
}
