package platform

// UserAllowed reports whether userID passes the access allowlist — the single
// rule for the sender of a message and the presser of a button on every chat
// transport (#2303). allowed is the resolved access.allowed_users map and
// allowedOnly the access.allowed_users_only flag (default true): only listed
// users pass, so an empty list blocks everyone; when explicitly false, an
// empty list allows anyone, while a non-empty list still filters. A nil map
// behaves like an empty one.
func UserAllowed(allowed map[string]bool, allowedOnly bool, userID string) bool {
	return allowed[userID] || (!allowedOnly && len(allowed) == 0)
}
