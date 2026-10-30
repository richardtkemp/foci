package skills

import (
	"path/filepath"
	"sync/atomic"

	"foci/internal/log"
)

var (
	skillsLog = log.NewComponentLogger("skills")
)

// Dirs is one agent's skill directories by role. The load order is Shared,
// then each Extra in the order listed, then Agent. A later directory wins a
// name clash (see LoadDirs), so the per-agent dir still beats everything and
// the shared dir still loses to everything — the pre-#2657 two-dir outcome.
type Dirs struct {
	Shared string   // [skills] dir, else $home/shared/skills
	Extra  []string // [skills] extra_dirs, then [agents.skills] extra_dirs (#2657)
	Agent  string   // skills_dir, else $workspace/skills
}

// List returns the directories in load order, skipping empty entries. The
// zero Dirs returns nil.
func (d Dirs) List() []string {
	var out []string
	for _, dir := range append(append([]string{d.Shared}, d.Extra...), d.Agent) {
		if dir != "" {
			out = append(out, dir)
		}
	}
	return out
}

// WithExtra returns d with Extra replaced by extra, minus empty entries,
// repeats, and any entry naming the Shared or Agent dir (compared after
// filepath.Clean). Dropping those keeps each directory at exactly one
// position in the load order, so its skills are scanned once and the
// clash outcome is decided by its role, not by a duplicate later copy.
// The first occurrence of a repeated extra dir keeps its position.
func (d Dirs) WithExtra(extra []string) Dirs {
	seen := map[string]bool{}
	if d.Shared != "" {
		seen[filepath.Clean(d.Shared)] = true
	}
	if d.Agent != "" {
		seen[filepath.Clean(d.Agent)] = true
	}
	var out []string
	for _, dir := range extra {
		if dir == "" {
			continue
		}
		c := filepath.Clean(dir)
		if seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, dir)
	}
	d.Extra = out
	return d
}

// ResolveDirs returns an agent's skill directories.
// home is the foci home directory (parent of agent workspaces).
// agentWorkspace is the agent's workspace path.
// sharedOverride, if non-empty, replaces the default shared dir.
// agentOverride, if non-empty, replaces the default per-agent dir.
// extra are ADDITIONAL dirs loaded between the two (already path-resolved by
// config.Load); see Dirs.WithExtra for what is dropped.
func ResolveDirs(home, agentWorkspace, sharedOverride, agentOverride string, extra []string) Dirs {
	sharedDir := filepath.Join(home, "shared", "skills")
	if sharedOverride != "" {
		sharedDir = sharedOverride
	}
	agentDir := filepath.Join(agentWorkspace, "skills")
	if agentOverride != "" {
		agentDir = agentOverride
	}
	return Dirs{Shared: sharedDir, Agent: agentDir}.WithExtra(extra)
}

// LiveDirs holds an agent's current Dirs so a live config edit of
// extra_dirs (#2657) reaches every reader without a restart: the skill
// reload at session start, and the reflection skill-change snapshots. All
// methods are safe on a nil *LiveDirs, which reads as the zero Dirs.
type LiveDirs struct {
	p atomic.Pointer[Dirs]
}

// NewLiveDirs returns a LiveDirs holding d.
func NewLiveDirs(d Dirs) *LiveDirs {
	l := &LiveDirs{}
	l.Set(d)
	return l
}

// Get returns the current Dirs (the zero Dirs for a nil receiver).
func (l *LiveDirs) Get() Dirs {
	if l == nil {
		return Dirs{}
	}
	if d := l.p.Load(); d != nil {
		return *d
	}
	return Dirs{}
}

// List is Get().List().
func (l *LiveDirs) List() []string {
	return l.Get().List()
}

// Set replaces the current Dirs. The Extra slice is copied, so the caller
// may reuse its own.
func (l *LiveDirs) Set(d Dirs) {
	d.Extra = append([]string(nil), d.Extra...)
	l.p.Store(&d)
}
