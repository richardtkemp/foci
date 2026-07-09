package askgw

import (
	"fmt"
	"os/user"
	"strconv"
)

func groupGID(name string) (int, error) {
	g, err := user.LookupGroup(name)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(g.Gid)
}

func resolveUID(s string) (uint32, error) {
	if n, err := strconv.ParseUint(s, 10, 32); err == nil {
		return uint32(n), nil
	}
	u, err := user.Lookup(s)
	if err != nil {
		return 0, fmt.Errorf("unknown user %q", s)
	}
	n, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("user %q has unparseable uid %q", s, u.Uid)
	}
	return uint32(n), nil
}
