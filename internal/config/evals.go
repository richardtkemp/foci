package config

// EvalsConfig configures the rubric registry: the axes traces can be scored
// on, read from files so a new axis needs no release. Scores go to the same
// Langfuse project as [tracing]; [evals] does nothing unless tracing is on.
type EvalsConfig struct {
	RubricsDir string `toml:"rubrics_dir" desc:"Directory of rubric files (one <name>.md with YAML front matter per scoring axis); watched for changes (default: <home>/shared/evals)"`
}
