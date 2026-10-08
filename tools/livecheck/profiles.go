package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"govoice/server/provider"
)

//go:embed profiles.yaml
var embeddedProfiles []byte

// Stage is one provider slot of a profile.
type Stage struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
	Language string `yaml:"language"`
	Voice    string `yaml:"voice"`
}

// Profile is one STT + LLM + TTS combination the standard agent runs on.
type Profile struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Stages      struct {
		STT Stage `yaml:"stt"`
		LLM Stage `yaml:"llm"`
		TTS Stage `yaml:"tts"`
	} `yaml:"stages"`
	Requires  []string                     `yaml:"requires"`
	Optional  []string                     `yaml:"optional"`
	Defaults  map[string]string            `yaml:"defaults"`
	BuildTags []string                     `yaml:"build_tags"`
	Infra     map[string]map[string]string `yaml:"infra"`
	Interims  string                       `yaml:"interims"` // yes | no | auto
	Fake      bool                         `yaml:"fake"`
	Notes     string                       `yaml:"notes"`
}

// providers is the server's own capability catalog. xAI STT/TTS are listed only in a
// -tags xaispeech build; the harness adds that tag itself (Profile.tags), so they are accepted.
var providers = provider.NewRegistry(provider.Definitions())

func serves(prov, stage string) bool {
	return (prov == "xai" && stage != "llm") || providers.Supports(prov, provider.Capability(stage))
}

func (p Profile) stages() map[string]Stage {
	return map[string]Stage{"stt": p.Stages.STT, "llm": p.Stages.LLM, "tts": p.Stages.TTS}
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func (p Profile) validate() error {
	if !nameRe.MatchString(p.Name) {
		return fmt.Errorf("profile %q: name must be lowercase letters, digits and dashes", p.Name)
	}
	for stage, s := range p.stages() {
		if !serves(s.Provider, stage) {
			return fmt.Errorf("profile %s: provider %q cannot serve %s", p.Name, s.Provider, stage)
		}
	}
	if strings.TrimSpace(p.Stages.LLM.Model) == "" {
		return fmt.Errorf("profile %s: llm.model is required (the server rejects an agent without it)", p.Name)
	}
	switch p.Interims {
	case "", "yes", "no", "auto":
	default:
		return fmt.Errorf("profile %s: interims must be yes, no or auto", p.Name)
	}
	for _, v := range p.Requires {
		if v != strings.ToUpper(v) || strings.ContainsAny(v, " =") {
			return fmt.Errorf("profile %s: requires entry %q is not an env var name", p.Name, v)
		}
	}
	// every variable the stage credentials read must be declared, or gating would pass and provisioning fail
	if !p.Fake {
		for stage, s := range p.stages() {
			cs := credSpecs[s.Provider]
			if cs == nil {
				return fmt.Errorf("profile %s: no credential handling for provider %q (add it in creds.go)", p.Name, s.Provider)
			}
			for _, v := range cs.requires(stage) {
				if !slices.Contains(p.Requires, v) {
					return fmt.Errorf("profile %s: %s uses %s but requires does not list it", p.Name, stage, v)
				}
			}
		}
	}
	if p.Fake && !slices.Contains(p.BuildTags, "fakeproviders") {
		return fmt.Errorf("profile %s: a fake profile must list build tag fakeproviders", p.Name)
	}
	return nil
}

// parseProfiles decodes and validates a profiles document (unknown keys are errors).
func parseProfiles(b []byte) ([]Profile, error) {
	var doc struct {
		Profiles []Profile `yaml:"profiles"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("profiles: %w", err)
	}
	seen := map[string]bool{}
	for _, p := range doc.Profiles {
		if err := p.validate(); err != nil {
			return nil, err
		}
		if seen[p.Name] {
			return nil, fmt.Errorf("profile %s: duplicate name", p.Name)
		}
		seen[p.Name] = true
	}
	return doc.Profiles, nil
}

// loadProfiles reads path, or the embedded profiles.yaml when path is empty.
func loadProfiles(path string) ([]Profile, error) {
	if path == "" {
		return parseProfiles(embeddedProfiles)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseProfiles(b)
}

// selectProfiles resolves "a,b" (or "all": every real profile) against all.
func selectProfiles(all []Profile, names string) ([]Profile, error) {
	var out []Profile
	for _, n := range splitList(names) {
		if n == "all" {
			for _, p := range all {
				if !p.Fake && !slices.ContainsFunc(out, func(o Profile) bool { return o.Name == p.Name }) {
					out = append(out, p)
				}
			}
			continue
		}
		i := slices.IndexFunc(all, func(p Profile) bool { return p.Name == n })
		if i < 0 {
			var known []string
			for _, p := range all {
				known = append(known, p.Name)
			}
			return nil, fmt.Errorf("unknown profile %q (known: %s)", n, strings.Join(known, ", "))
		}
		if !slices.ContainsFunc(out, func(o Profile) bool { return o.Name == n }) {
			out = append(out, all[i])
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no profiles selected")
	}
	if slices.ContainsFunc(out, func(p Profile) bool { return p.Fake }) && len(out) > 1 {
		return nil, fmt.Errorf("the fake profile replaces google.* with fakes in the server binary; run it alone")
	}
	return out, nil
}

func splitList(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// lookup returns an env lookup that falls back to the profile's defaults.
func (p Profile) lookup(env func(string) string) func(string) string {
	return func(k string) string {
		if v := env(k); v != "" {
			return v
		}
		return p.Defaults[k]
	}
}

var varRe = regexp.MustCompile(`\$\{([A-Z0-9_]+)(?::-([^}]*))?\}`)

// expand substitutes ${VAR} and ${VAR:-default}.
func expand(s string, lookup func(string) string) string {
	return varRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := varRe.FindStringSubmatch(m)
		if v := lookup(sub[1]); v != "" {
			return v
		}
		return sub[2]
	})
}

// resolve returns the profile with variables expanded: the effective config of a run.
func (p Profile) resolve(env func(string) string) Profile {
	lk := p.lookup(env)
	ex := func(s Stage) Stage {
		return Stage{Provider: s.Provider, Model: expand(s.Model, lk), Language: expand(s.Language, lk), Voice: expand(s.Voice, lk)}
	}
	r := p
	r.Stages.STT, r.Stages.LLM, r.Stages.TTS = ex(p.Stages.STT), ex(p.Stages.LLM), ex(p.Stages.TTS)
	r.Infra = map[string]map[string]string{}
	for k, m := range p.Infra {
		r.Infra[k] = map[string]string{}
		for kk, v := range m {
			if v = expand(v, lk); v != "" {
				r.Infra[k][kk] = v
			}
		}
	}
	return r
}

// gate returns why the profile cannot run with this environment, or "".
func (p Profile) gate(env func(string) string) string {
	if p.Fake {
		return ""
	}
	lk := p.lookup(env)
	var missing []string
	for _, v := range p.Requires {
		if lk(v) == "" {
			missing = append(missing, v)
		}
	}
	if len(missing) > 0 {
		return "missing env: " + strings.Join(missing, ", ")
	}
	for stage, s := range p.stages() {
		if _, err := credSpecs[s.Provider].secret(stage, lk); err != nil {
			return fmt.Sprintf("%s credential: %v", stage, err)
		}
	}
	return ""
}

// impliedTags are build tags a stage needs regardless of what the profile declares.
func (p Profile) tags() []string {
	t := slices.Clone(p.BuildTags)
	for stage, s := range p.stages() {
		if s.Provider == "xai" && stage != "llm" {
			t = append(t, "xaispeech")
		}
	}
	return t
}

// tagUnion is the sorted set of build tags the server binary needs for ps.
func tagUnion(ps []Profile) []string {
	set := map[string]bool{}
	for _, p := range ps {
		for _, t := range p.tags() {
			set[t] = true
		}
	}
	var out []string
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
