package chatmeta

import "hash/fnv"

// StableChatID maps an arbitrary stable string identity — an app conversation
// id, a Slack dm/thread identity — to a stable positive int64 chat ID, so the
// same identity resolves to the same foci session (agent/c<chatID>) across
// restarts. FNV-64a; collision probability is negligible for the handful of
// conversations one deployment holds.
func StableChatID(identity string) int64 {
	hsh := fnv.New64a()
	_, _ = hsh.Write([]byte(identity))
	v := int64(hsh.Sum64() & 0x7fffffffffffffff) //nolint:gosec // masked to 63 bits, always fits int64
	if v == 0 {
		v = 1
	}
	return v
}
