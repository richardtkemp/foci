package skills

// Load scans dirs in order with no directory roles (every clash is an INFO
// "override", the later dir wins). Production loads by role through LoadDirs
// / Loader.LoadForAgent (#2657); this role-less form only serves the tests
// that predate roles, so it lives here rather than as dead production code.
func Load(dirs []string) *Registry {
	m := newMerger(&Registry{})
	for _, dir := range dirs {
		m.add(scanDir(dir), false)
	}
	return m.r
}
