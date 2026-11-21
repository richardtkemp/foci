package configcheck

import (
	"errors"
	"os/user"
	"strings"
	"testing"

	"foci/internal/config"
)

// fakeAskgwDeps: the gateway user "gw" (uid 500, primary gid 500) is in the
// groups listed in member; the groups that exist are listed in groups.
func fakeAskgwDeps(euid, dirOwner int, groups map[string]string, member []string) askgwGroupDeps {
	return askgwGroupDeps{
		euid:     euid,
		ownerUID: func(string) (int, error) { return dirOwner, nil },
		lookupID: func(uid string) (*user.User, error) {
			if uid != "500" {
				return nil, errors.New("unknown uid " + uid)
			}
			return &user.User{Uid: "500", Gid: "500", Username: "gw"}, nil
		},
		lookupGroup: func(name string) (*user.Group, error) {
			gid, ok := groups[name]
			if !ok {
				return nil, user.UnknownGroupError(name)
			}
			return &user.Group{Gid: gid, Name: name}, nil
		},
		groupIDs: func(*user.User) ([]string, error) { return append([]string{"500"}, member...), nil },
	}
}

func askgwCfg(enabled bool, group string) *config.Config {
	return &config.Config{Askgw: config.AskgwConfig{Enabled: enabled, Group: group}}
}

func TestAskgwGroupConcern(t *testing.T) {
	groups := map[string]string{"foci-askgw": "501", "gw": "500", "other": "600"}
	cases := []struct {
		name     string
		cfg      *config.Config
		euid     int
		dirOwner int
		member   []string
		want     []string // substrings; nil = no finding
	}{
		{"not a member (the work Mac repro)", askgwCfg(true, ""), 500, 0, nil,
			[]string{`"foci-askgw"`, "gw is not a member", "0600"}},
		{"member", askgwCfg(true, ""), 500, 0, []string{"501"}, nil},
		{"primary group", askgwCfg(true, "gw"), 500, 0, nil, nil},
		{"missing group", askgwCfg(true, "nope"), 500, 0, nil,
			[]string{`"nope" does not exist`, "0600"}},
		{"disabled", askgwCfg(false, "other"), 500, 0, nil, nil},
		{"root check judges the config dir owner", askgwCfg(true, "other"), 0, 500, []string{"501"},
			[]string{`"other"`, "gw is not a member"}},
		{"root check, root-owned dir: no finding", askgwCfg(true, "other"), 0, 0, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := askgwGroupConcern(tc.cfg, "/x/config/foci.toml", fakeAskgwDeps(tc.euid, tc.dirOwner, groups, tc.member))
			if tc.want == nil {
				if got != "" {
					t.Fatalf("want no finding, got %q", got)
				}
				return
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("finding %q lacks %q", got, w)
				}
			}
		})
	}
}
