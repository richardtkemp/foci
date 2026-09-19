package evals

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"foci/internal/log"
)

var evalsLog = log.NewComponentLogger("evals")

// Registry is the live set of rubrics from one directory. Load once, then
// Watch to follow edits; every read goes through the lock, so a reload is
// atomic to callers.
type Registry struct {
	dir string

	mu      sync.RWMutex
	rubrics map[string]*Rubric
	errs    map[string]error // file → last load error (bad files are skipped, not fatal)
	loaded  time.Time

	// OnChange is called after each successful (re)load with the rubrics
	// that were added or changed since the previous load — the gateway uses
	// it to mirror score configs. Set before Watch.
	OnChange func(changed []*Rubric)

	watcher *fsnotify.Watcher
	timer   *time.Timer
}

// ResolveDir returns the rubrics directory: the configured override, else
// <home>/shared/evals (home = the parent of the agent workspaces, as for
// skills).
func ResolveDir(home, override string) string {
	if override != "" {
		return override
	}
	return filepath.Join(home, "shared", "evals")
}

// Load reads every *.md in dir. A missing directory is an empty registry,
// not an error — evals are opt-in. Per-file errors are kept (Errors) and
// logged; the rest of the directory still loads.
func Load(dir string) (*Registry, error) {
	r := &Registry{dir: dir, rubrics: map[string]*Rubric{}, errs: map[string]error{}}
	if err := r.reload(); err != nil {
		return nil, err
	}
	return r, nil
}

// Dir is the watched directory.
func (r *Registry) Dir() string { return r.dir }

func (r *Registry) reload() error {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		if os.IsNotExist(err) {
			entries = nil
		} else {
			return fmt.Errorf("read rubrics dir %s: %w", r.dir, err)
		}
	}
	var changed []*Rubric
	next := map[string]*Rubric{}
	errs := map[string]error{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		path := filepath.Join(r.dir, e.Name())
		content, rerr := os.ReadFile(path)
		if rerr != nil {
			errs[path] = rerr
			continue
		}
		stem := strings.TrimSuffix(e.Name(), ".md")
		rb, perr := Parse(path, stem, content)
		if perr != nil {
			errs[path] = perr
			evalsLog.Warnf("rubric %s skipped: %v", e.Name(), perr)
			continue
		}
		next[rb.Name] = rb
	}

	r.mu.Lock()
	prev := r.rubrics
	for name, rb := range next {
		old, had := prev[name]
		if !had || old.Version != rb.Version || old.Type != rb.Type || old.Kind != rb.Kind || old.Body != rb.Body || old.Description != rb.Description {
			changed = append(changed, rb)
		} else {
			rb.ConfigID = old.ConfigID // carry the mirrored config across an unchanged reload
		}
	}
	var removed []string
	for name := range prev {
		if _, still := next[name]; !still {
			removed = append(removed, name)
		}
	}
	r.rubrics = next
	r.errs = errs
	r.loaded = time.Now()
	r.mu.Unlock()

	sort.Strings(removed)
	sortRubrics(changed)
	if len(changed) > 0 || len(removed) > 0 {
		names := make([]string, 0, len(changed))
		for _, rb := range changed {
			names = append(names, fmt.Sprintf("%s@%d", rb.Name, rb.Version))
		}
		evalsLog.Infof("rubrics loaded from %s: %d total, changed [%s], removed [%s], %d bad file(s)",
			r.dir, len(next), strings.Join(names, " "), strings.Join(removed, " "), len(errs))
	}
	if r.OnChange != nil && len(changed) > 0 {
		r.OnChange(changed)
	}
	return nil
}

// Get returns the rubric named name.
func (r *Registry) Get(name string) (*Rubric, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rb, ok := r.rubrics[name]
	return rb, ok
}

// List returns every rubric, sorted by name.
func (r *Registry) List() []*Rubric {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Rubric, 0, len(r.rubrics))
	for _, rb := range r.rubrics {
		out = append(out, rb)
	}
	sortRubrics(out)
	return out
}

// Human returns the human-graded rubrics that apply to agent and
// sessionType — the set a score control or /score listing shows.
func (r *Registry) Human(agent, sessionType string) []*Rubric {
	var out []*Rubric
	for _, rb := range r.List() {
		if rb.Kind == KindHuman && rb.Select.Matches(agent, "", sessionType, "", nil, "") {
			out = append(out, rb)
		}
	}
	return out
}

// Errors returns the files that failed to load, by path.
func (r *Registry) Errors() map[string]error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]error, len(r.errs))
	for k, v := range r.errs {
		out[k] = v
	}
	return out
}

// SetConfigID records the mirrored Langfuse score config for name.
func (r *Registry) SetConfigID(name, id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rb, ok := r.rubrics[name]; ok {
		rb.ConfigID = id
	}
}

// Watch follows the directory with fsnotify and reloads (debounced 500 ms)
// on any change to a .md file. The directory is created if missing so the
// watch has something to attach to — an operator dropping the first rubric
// in should not need a restart. Close stops it.
func (r *Registry) Watch() error {
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		return fmt.Errorf("create rubrics dir: %w", err)
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	if err := w.Add(r.dir); err != nil {
		_ = w.Close()
		return fmt.Errorf("watch %s: %w", r.dir, err)
	}
	r.watcher = w
	go func() {
		for {
			select {
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if filepath.Ext(ev.Name) == ".md" || ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename) {
					r.schedule()
				}
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				evalsLog.Warnf("rubrics watcher: %v", err)
			}
		}
	}()
	return nil
}

func (r *Registry) schedule() {
	r.mu.Lock()
	if r.timer != nil {
		r.timer.Stop()
	}
	r.timer = time.AfterFunc(500*time.Millisecond, func() {
		if err := r.reload(); err != nil {
			evalsLog.Errorf("rubrics reload: %v", err)
		}
	})
	r.mu.Unlock()
}

// Close stops the watcher.
func (r *Registry) Close() {
	r.mu.Lock()
	if r.timer != nil {
		r.timer.Stop()
	}
	w := r.watcher
	r.watcher = nil
	r.mu.Unlock()
	if w != nil {
		_ = w.Close()
	}
}
