// Package catalog describes the providers go-voice can use: which environment
// variables carry their secrets, which services each unlocks, and the curated
// models and voices. It holds variable NAMES only; values stay in the process
// environment (see Resolver).
package catalog

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed data/*.yaml
var packaged embed.FS

const (
	ServiceSTT = "stt"
	ServiceLLM = "llm"
	ServiceTTS = "tts"

	maxEntries = 500
	maxIDLen   = 128
	// MaxFileBytes caps one data file (packaged or overlay).
	MaxFileBytes = 64 << 10
)

var (
	providerID  = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	envName     = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	assemblers  = map[string]bool{"token": true, "json": true, "file_json": true}
	serviceName = map[string]bool{ServiceSTT: true, ServiceLLM: true, ServiceTTS: true}
)

type Catalog struct {
	byID map[string]*Provider
}

type Provider struct {
	ID          string              `yaml:"id"`
	Name        string              `yaml:"name"`
	Credentials []Kind              `yaml:"credentials"`
	Services    map[string]*Service `yaml:"services"`
}

type Kind struct {
	ID       string           `yaml:"id"`
	Label    string           `yaml:"label"`
	Fields   map[string]Field `yaml:"fields"`
	Assemble string           `yaml:"assemble"`
}

type Field struct {
	Env      string `yaml:"env"`
	Optional bool   `yaml:"optional"`
}

type Service struct {
	Credential []string `yaml:"credential"`
	Needs      []string `yaml:"needs"`
	Models     []Entry  `yaml:"models"`
	Voices     []Entry  `yaml:"voices"`
	Languages  []Entry  `yaml:"languages"`
	Custom     *bool    `yaml:"custom"`
}

type Entry struct {
	ID      string `yaml:"id"`
	Label   string `yaml:"label"`
	Default bool   `yaml:"default"`
}

// AllowsCustom reports whether the UI may offer an "Other..." free-text entry.
func (s *Service) AllowsCustom() bool { return s.Custom == nil || *s.Custom }

// DefaultEntry is the list's default, if it has one.
func DefaultEntry(es []Entry) (Entry, bool) {
	for _, e := range es {
		if e.Default {
			return e, true
		}
	}
	return Entry{}, false
}

// Kind returns the provider's secret kind with that id.
func (p *Provider) Kind(id string) (*Kind, bool) {
	for i := range p.Credentials {
		if p.Credentials[i].ID == id {
			return &p.Credentials[i], true
		}
	}
	return nil, false
}

func (c *Catalog) Provider(id string) (*Provider, bool) {
	p, ok := c.byID[id]
	return p, ok
}

func (c *Catalog) Providers() []*Provider {
	out := make([]*Provider, 0, len(c.byID))
	for _, p := range c.byID {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Load reads the packaged provider files. A packaged file that does not
// validate is a bug in the release, so any error is returned.
func Load() (*Catalog, error) {
	entries, err := fs.ReadDir(packaged, "data")
	if err != nil {
		return nil, err
	}
	c := &Catalog{byID: map[string]*Provider{}}
	for _, e := range entries {
		data, err := packaged.ReadFile("data/" + e.Name())
		if err != nil {
			return nil, err
		}
		p, err := Parse(e.Name(), data)
		if err != nil {
			return nil, err
		}
		if _, dup := c.byID[p.ID]; dup {
			return nil, fmt.Errorf("%s: duplicate provider id %q", e.Name(), p.ID)
		}
		c.byID[p.ID] = p
	}
	return c, nil
}

// Parse decodes one provider file strictly (unknown keys are errors) and validates it.
func Parse(name string, data []byte) (*Provider, error) { return parse(name, data, false) }

// parse with overlay=true accepts an operator file, which carries no name or
// service credentials (merge checks those against the packaged provider).
func parse(name string, data []byte, overlay bool) (*Provider, error) {
	if len(data) > MaxFileBytes {
		return nil, fmt.Errorf("%s: larger than %d bytes", name, MaxFileBytes)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var p Provider
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if err := p.validate(overlay); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return &p, nil
}

func (p *Provider) validate(overlay bool) error {
	if !providerID.MatchString(p.ID) {
		return fmt.Errorf("provider id %q must match %s", p.ID, providerID)
	}
	if !overlay && strings.TrimSpace(p.Name) == "" {
		return errors.New("name is required")
	}
	kinds := map[string]*Kind{}
	for i := range p.Credentials {
		k := &p.Credentials[i]
		if !providerID.MatchString(k.ID) || kinds[k.ID] != nil {
			return fmt.Errorf("credential kind id %q must be unique and match %s", k.ID, providerID)
		}
		kinds[k.ID] = k
		if strings.TrimSpace(k.Label) == "" {
			return fmt.Errorf("credential kind %q needs a label", k.ID)
		}
		if !assemblers[k.Assemble] {
			return fmt.Errorf("credential kind %q: unknown assemble %q", k.ID, k.Assemble)
		}
		if len(k.Fields) == 0 {
			return fmt.Errorf("credential kind %q needs at least one field", k.ID)
		}
		if (k.Assemble == "token" || k.Assemble == "file_json") && len(k.Fields) != 1 {
			return fmt.Errorf("credential kind %q: %s takes exactly one field", k.ID, k.Assemble)
		}
		for id, f := range k.Fields {
			if id == "" || !envName.MatchString(f.Env) {
				return fmt.Errorf("credential kind %q field %q: env %q must match %s", k.ID, id, f.Env, envName)
			}
		}
	}
	for name, svc := range p.Services {
		if !serviceName[name] || svc == nil {
			return fmt.Errorf("unknown service %q", name)
		}
		if len(svc.Credential) == 0 && !overlay {
			return fmt.Errorf("service %q needs at least one credential kind", name)
		}
		for _, kid := range svc.Credential {
			if kinds[kid] == nil {
				return fmt.Errorf("service %q: unknown credential kind %q", name, kid)
			}
		}
		for _, need := range svc.Needs {
			ok := false
			for _, kid := range svc.Credential {
				_, ok = kinds[kid].Fields[need]
				if ok {
					break
				}
			}
			if !ok {
				return fmt.Errorf("service %q: needs unknown field %q", name, need)
			}
		}
		for what, list := range map[string][]Entry{"models": svc.Models, "voices": svc.Voices, "languages": svc.Languages} {
			if err := validateEntries(name+" "+what, list); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateEntries(what string, es []Entry) error {
	if len(es) > maxEntries {
		return fmt.Errorf("%s: more than %d entries", what, maxEntries)
	}
	seen, defaults := map[string]bool{}, 0
	for _, e := range es {
		if e.ID == "" || len(e.ID) > maxIDLen || seen[e.ID] || strings.TrimSpace(e.Label) == "" {
			return fmt.Errorf("%s: entry %q needs a unique id (at most %d characters) and a label", what, e.ID, maxIDLen)
		}
		seen[e.ID] = true
		if e.Default {
			defaults++
		}
	}
	if defaults > 1 {
		return fmt.Errorf("%s: more than one default", what)
	}
	return nil
}
