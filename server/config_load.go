package server

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// LoadConfig parses and validates YAML configuration from disk.
func LoadConfig(path string) (*Config, error) { return LoadConfigProfile(path, "") }

// LoadConfigProfile is LoadConfig with the YAML profile overridden (the
// -profile flag): the override picks the defaults and is what gets validated,
// so -profile production cannot inherit development defaults or skip checks.
func LoadConfigProfile(path string, profile Profile) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	if err := rejectRemovedKeys(data); err != nil {
		return nil, err
	}

	// Defaults depend on the profile, so read it first. yaml.v3 decodes into the
	// pre-populated struct, replacing only keys present (lists are replaced
	// whole, maps merge, an explicit zero overwrites).
	var head struct {
		Profile Profile `yaml:"profile"`
	}
	_ = yaml.Unmarshal(data, &head) // syntax errors are reported by the strict decode
	if profile != "" {
		head.Profile = profile
	}
	cfg := DefaultConfig(head.Profile)
	if head.Profile == ProfileProduction {
		cfg.OriginAllowlist, cfg.Storage.Dialect = nil, ""
	}
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true) // Reject unknown fields per spec §28 & Phase 8 checklist
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse config yaml: %w", err)
	}
	cfg.Profile = head.Profile

	if err := ValidateConfig(cfg); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	return cfg, nil
}

// rejectRemovedKeys turns keys removed with YAML pipelines into an actionable
// error instead of a generic unknown-field failure.
func rejectRemovedKeys(data []byte) error {
	var top map[string]any
	if err := yaml.Unmarshal(data, &top); err != nil {
		return nil // the strict decode reports syntax errors
	}
	var doc struct {
		Providers map[string]map[string]any `yaml:"providers"`
		Overrides map[string]any            `yaml:"insecure_overrides"`
	}
	_ = yaml.Unmarshal(data, &doc)
	if _, ok := doc.Overrides["skip_credential_perms_check"]; ok {
		return errors.New("config: insecure_overrides.skip_credential_perms_check is no longer supported; there is no credential file check. Remove the key")
	}
	if _, ok := top["pipelines"]; ok {
		return errors.New("config: `pipelines` is no longer supported; calls run stored agents. Create agents via /api/v1/agents and remove the pipelines section")
	}
	for provider, settings := range doc.Providers {
		for _, key := range []string{"max_streams", "rotation_headroom_multiplier"} {
			if _, ok := settings[key]; ok {
				return fmt.Errorf("config: providers.%s.%s is no longer supported; limits.max_sessions caps concurrent calls (keep it within your STT stream quota)", provider, key)
			}
		}
		for _, key := range []string{"model", "language_code", "language", "voice", "system_prompt", "api_key_env", "credentials_file", "auth_mode"} {
			if _, ok := settings[key]; ok {
				return fmt.Errorf("config: providers.%s.%s is no longer supported; models, voices, prompts and credentials belong to stored agents and provider credentials", provider, key)
			}
		}
	}
	return nil
}
