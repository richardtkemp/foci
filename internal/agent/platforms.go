package agent

import (
	"foci/internal/platform"
)

func (a *Agent) AddPlatform(name string, p platform.Sender) {
	a.platformMu.Lock()
	defer a.platformMu.Unlock()
	if a.platforms == nil {
		a.platforms = make(map[string]platform.Sender)
	}
	a.platforms[name] = p
}
