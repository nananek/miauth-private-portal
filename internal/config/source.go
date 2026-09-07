package config

import "os"

// Source names where a known key's bootstrap (non-DB) value came from.
// It is independent of ADR-0006's DB overlay — see ClassOf/
// IsDBEligibleKey for that layer — and only describes Load's own
// existing file/env/default precedence.
type Source string

const (
	SourceEnv     Source = "env"
	SourceFile    Source = "file"
	SourceDefault Source = "default"
)

// Sources reports, for every known key, whether Load's file/env/default
// resolution would take its bootstrap value from an environment
// variable, the config file, or neither (default) — the same precedence
// Load itself applies (an env var wins over a file value, which wins
// over the default), computed without re-running parse()/Validate. Load
// itself only needs each key's final merged value, not this per-key
// provenance, which exists for miauthctl config list/get's display
// (ADR-0006 §2-5).
func Sources(opts LoadOptions) (map[string]Source, error) {
	if opts.Getenv == nil {
		opts.Getenv = os.LookupEnv
	}

	fileValues := map[string]string{}
	if opts.ConfigFilePath != "" {
		fv, err := loadConfigFile(opts.ConfigFilePath)
		if err != nil {
			return nil, err
		}
		fileValues = fv
	}

	sources := make(map[string]Source, len(knownKeyOrder))
	for _, key := range knownKeyOrder {
		if v, ok := opts.Getenv(key); ok && v != "" {
			sources[key] = SourceEnv
			continue
		}
		if v, ok := fileValues[key]; ok && v != "" {
			sources[key] = SourceFile
			continue
		}
		sources[key] = SourceDefault
	}
	return sources, nil
}
