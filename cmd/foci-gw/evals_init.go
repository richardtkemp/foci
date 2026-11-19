package main

import (
	"path/filepath"

	"foci/internal/clock"
	"foci/internal/config"
	"foci/internal/evals"
	"foci/internal/log"
	"foci/internal/telemetry"
)

var evalsInitLog = log.NewComponentLogger("evals")

// initEvals loads the rubric registry and follows its directory. Rubrics are
// data: an operator drops a file in and the axis exists — in the registry
// immediately (fsnotify), and in Langfuse as a score config so the UI can
// render it (mirrored on every load that adds or changes a rubric). With
// tracing off the registry still loads (so /score can validate and the CLI
// can list) but nothing is mirrored and no score can be posted.
func initEvals(cfg *config.Config) (*evals.Registry, func()) {
	home := ""
	if len(cfg.Agents) > 0 {
		home = filepath.Dir(cfg.Agents[0].Workspace)
	}
	dir := evals.ResolveDir(home, cfg.Evals.RubricsDir)
	reg, err := evals.Load(dir)
	if err != nil {
		evalsInitLog.Errorf("rubrics disabled: %v", err)
		return nil, func() {}
	}
	// One mirror for the process: a reload enqueues into it and joins its
	// retry loop rather than starting another (#3189). Drift between a
	// rubric and an already-existing config is logged, never repaired — the
	// API cannot update a config, and silently posting against a
	// differently-shaped one would be worse than a warning.
	mirror := newScoreConfigMirror(reg, telemetry.EnsureScoreConfig, clock.Real(), evalsInitLog)
	enqueue := func(rs []*evals.Rubric) {
		if telemetry.ScoringAvailable() {
			mirror.Enqueue(rs)
		}
	}
	reg.OnChange = enqueue
	if err := reg.Watch(); err != nil {
		evalsInitLog.Warnf("rubrics dir %s not watched (edits need a restart): %v", dir, err)
	}
	enqueue(reg.List())
	if n := len(reg.List()); n > 0 || len(reg.Errors()) > 0 {
		evalsInitLog.Infof("%d rubric(s) from %s (%d bad file(s))", n, dir, len(reg.Errors()))
	}
	return reg, func() {
		reg.Close()
		mirror.Close()
	}
}

// scoreConfigFor maps a rubric's shape onto Langfuse's config vocabulary.
func scoreConfigFor(rb *evals.Rubric) telemetry.ScoreConfig {
	c := telemetry.ScoreConfig{Name: rb.Name, Description: rb.Description}
	switch rb.Type {
	case evals.TypeNumeric:
		c.DataType = telemetry.ScoreNumeric
		c.MinValue, c.MaxValue = rb.Min, rb.Max
	case evals.TypeBoolean:
		c.DataType = telemetry.ScoreBoolean
	case evals.TypeCategorical:
		c.DataType = telemetry.ScoreCategorical
		for _, cat := range rb.Categories {
			c.Categories = append(c.Categories, telemetry.ScoreCategory{Label: cat.Label, Value: cat.Value})
		}
	}
	return c
}
