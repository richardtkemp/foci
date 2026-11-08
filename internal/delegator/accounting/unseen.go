package accounting

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// invUnseenField (#2111 §8.6): every adapter decodes a provider's usage object
// into a fixed struct, so a key the provider adds — a new token class, a new
// rate tier — is dropped without a trace, and the call is priced as if it were
// not there. Each adapter therefore compares the raw usage object with the
// keys it knows, and a key it does not know raises InvUnseenField. The call is
// still booked as before: the alarm says the booking may be short, it does not
// stop it.

// UsageKeys is the usage schema an adapter knows. Each known key maps to:
//   - nil: a leaf, whatever its value;
//   - a UsageKeys: an object whose own keys are checked in turn (a value that
//     is not an object, such as null, is not checked);
//   - AnyUsage: content foci knows of and does not read (CC's iterations).
type UsageKeys map[string]UsageKeys

// AnyUsage marks a known key whose content is not checked.
var AnyUsage = UsageKeys{anyUsageKey: nil}

const anyUsageKey = "\x00any"

// UnseenKey is one usage key an adapter did not expect: a key it does not
// know (Reason ""), or a known key whose value foci cannot price (Reason says
// why).
type UnseenKey struct {
	Path   string // dotted, nested keys included: "cache_creation.ephemeral_24h_input_tokens"
	Value  string // the raw JSON value, compacted
	Reason string
}

func (k UnseenKey) String() string {
	s := k.Path + "=" + k.Value
	if k.Reason != "" {
		s += " (" + k.Reason + ")"
	}
	return s
}

// dedupeKey is what makes two sightings the same: an unknown key is the same
// key whatever its value (a count differs on every call); a known key with an
// unpriceable value is new for each new value.
func (k UnseenKey) dedupeKey() string {
	if k.Reason == "" {
		return k.Path
	}
	return k.Path + "=" + k.Value
}

// UnseenUsageKeys returns every key of the usage object raw that known does
// not list, sorted by path, except one whose value counts nothing
// (countsNothing: a zero count is not a booking left short). raw that is empty, null or not an object holds
// no keys.
func UnseenUsageKeys(raw []byte, known UsageKeys) []UnseenKey {
	var out []UnseenKey
	unseenIn(raw, known, "", &out)
	slices.SortFunc(out, func(a, b UnseenKey) int { return strings.Compare(a.Path, b.Path) })
	return out
}

func unseenIn(raw []byte, known UsageKeys, prefix string, out *[]UnseenKey) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return
	}
	for _, k := range slices.Sorted(maps.Keys(obj)) {
		sub, ok := known[k]
		switch {
		case !ok:
			if countsNothing(obj[k]) {
				continue
			}
			*out = append(*out, UnseenKey{Path: prefix + k, Value: compactJSON(obj[k])})
		case sub == nil:
		case isAny(sub):
		default:
			unseenIn(obj[k], sub, prefix+k+".", out)
		}
	}
}

// countsNothing reports whether an unknown key's value leaves nothing
// unpriced (#2982): the number 0, or an object whose every value counts
// nothing (an empty object included). Such a key is not reported, and so not
// marked seen either: the first call that carries a count for it still alarms.
// Any other value — a non-zero count, a string such as a tier, null, a bool,
// an array — is reported.
func countsNothing(raw json.RawMessage) bool {
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		f, err := n.Float64()
		return err == nil && f == 0
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return false
	}
	for _, v := range obj {
		if !countsNothing(v) {
			return false
		}
	}
	return true
}

func isAny(k UsageKeys) bool {
	_, ok := k[anyUsageKey]
	return ok
}

// StandardTierOnly checks the rate-tier keys of an Anthropic usage object:
// foci prices standard rates only (no fast mode, no priority tier), so a
// `speed` or `service_tier` other than "standard" — absent or null is
// standard — is a call priced at the wrong rate. Each such key is returned.
func StandardTierOnly(raw []byte) []UnseenKey {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	var out []UnseenKey
	for _, k := range []string{"service_tier", "speed"} {
		v, ok := obj[k]
		if !ok {
			continue
		}
		var s *string
		if json.Unmarshal(v, &s) == nil && (s == nil || *s == "standard") {
			continue
		}
		out = append(out, UnseenKey{Path: k, Value: compactJSON(v),
			Reason: "foci has standard rates only, so this call is priced at the standard rate"})
	}
	return out
}

func compactJSON(raw json.RawMessage) string {
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return string(raw)
	}
	return b.String()
}

// AlarmUnseen raises InvUnseenField for the keys adapter found in one call's
// usage that this ledger has not alarmed on before: the same key seen again in
// this process (a history copy, the next call) does not alarm twice. backend
// and version are the alarm's; adapter names the decoder ("claude-code
// transcript", "anthropic", ...); call says which call it was. Nil-safe: with
// no ledger nothing is raised.
func (l *Ledger) AlarmUnseen(backend, version, adapter, call string, keys []UnseenKey) {
	if l == nil || len(keys) == 0 {
		return
	}
	l.unseenMu.Lock()
	if l.unseen == nil {
		l.unseen = map[string]bool{}
	}
	var fresh []string
	for _, k := range keys {
		id := adapter + "\x00" + k.dedupeKey()
		if l.unseen[id] {
			continue
		}
		l.unseen[id] = true
		fresh = append(fresh, k.String())
	}
	l.unseenMu.Unlock()
	if len(fresh) == 0 {
		return
	}
	l.Alarm(Alarm{Invariant: InvUnseenField, Backend: backend, Version: version,
		Detail: fmt.Sprintf("%s usage of %s carries what foci does not price: %s — the call is booked without it",
			adapter, call, strings.Join(fresh, "; "))})
}
