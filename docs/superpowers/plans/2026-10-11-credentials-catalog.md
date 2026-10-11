# Credentials Epic (Provider Catalog, Environment-Only Credentials) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Provider secrets are never stored: they are read from the process environment when a call needs them, a packaged YAML catalog maps variables to providers and services and holds the curated models and voices, and the UI offers only what is detected.

**Architecture:** A new `server/catalog` package (strict YAML, embedded per-provider files, operator overlay, validation) and a `catalog.Resolver` (live detection from an injected `getenv`, three secret assemblers) replace the whole credential store. Agents stop naming a credential: the secret is found by provider and service. A small `agent.Availability` interface (implemented in `server` from the catalog plus the adapter factories) feeds readiness and the call path. MCP bearer tokens come from `MCP_TOKEN_*` variables. The UI gets a read-only provider status page and one shared `StageFields` component with catalog dropdowns.

**Tech Stack:** Go 1.x (`server` is its own module), `gopkg.in/yaml.v3` (already a dependency), `go:embed`, React 19 + TanStack Query + zod + vitest, Playwright.

**Spec:** [docs/superpowers/specs/2026-10-11-credentials-catalog-design.md](../specs/2026-10-11-credentials-catalog-design.md)

## Global Constraints

- Secrets are never persisted, returned by any API, logged, traced or put on a timeline. Errors, readiness and API responses name variables, never values.
- The database is assumed clean: edit `001_initial.sql` for both dialects in place; no data migration, no drop migration. Developers delete `.govoice-dev` once.
- `core/` stays server-agnostic (nothing in this plan touches `core/`).
- Detection is evaluated each time it is needed, never cached.
- `file_json` files are capped at 64 KiB; overlay files at 64 KiB and 500 entries per list; ids at most 128 characters.
- Catalog env names match `^[A-Z][A-Z0-9_]*$`; MCP token variables match `^MCP_TOKEN_[A-Z0-9_]{1,48}$`.
- All Go code passes `go test -race` for `core`, `server` and `tools/livecheck`; `gofmt -w .` and `make verify` before the final review.
- Commits are conventional and end with `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`.
- Existing graph-agent behaviour, the cue, and the fake-provider e2e suites keep working (their credential setup changes, their assertions do not).

## Review Focus

1. **A secret value reaches an API response, log line, trace attribute or timeline** → a sentinel secret in the environment never appears in any API body, server log or trace span (Task 12 `TestNoSecretLeavesTheServer`).
2. **The variable is removed or changed between publish and call, or between two graph nodes** → the call is refused with a clear not-ready reason naming the variable, no panic, and the next resolve sees the new value (Tasks 3, 6).
3. **MCP token variable names that escape the prefix**: `TOKEN_SECRET`, `mcp_token_x`, `MCP_TOKEN_` (empty suffix), `MCP_TOKEN_A B`, 49-character suffix → all rejected (Task 8).
4. **A hostile overlay file**: oversize, unknown keys, tries to override `credentials`/`assemble`, adds a provider with no adapter, duplicate ids → the server starts, the file is skipped and logged, packaged data is unchanged (Task 2).
5. **A stale client posts `credential_id`** (agent create/update, fallbacks, graph nodes) → 400 `invalid_config` naming the unknown field (Task 5).
6. **Google with only one of its two secret kinds**: only `GOOGLE_API_KEY` → LLM available, STT and TTS not (`missing_env: [GOOGLE_APPLICATION_CREDENTIALS]`); only the service account → all three available (Task 3).
7. **`file_json` problems**: path unreadable, not JSON, over 64 KiB, a directory → the kind is not detected and the reason names the variable (Task 3).

---

## File Structure

| File | Responsibility |
|---|---|
| `server/catalog/catalog.go` (new) | Types, strict parse, validation, embedded loading |
| `server/catalog/data/{google,xai,elevenlabs,deepgram,cartesia,openai,nvidia}.yaml` (new) | Packaged provider data |
| `server/catalog/overlay.go` (new) | Operator overlay merge |
| `server/catalog/resolver.go` (new) | Live detection, assemblers, secrets, status |
| `server/providers.go` (new) | `providerAvailability` (catalog + resolver + adapter factories) |
| `server/api/providers.go` (rewrite) | Read-only `GET /api/v1/providers` |
| `server/agent/{config,fallback,graph,service}.go` | No `credential_id`; `Availability`; `missing_env` |
| `server/call.go`, `server/graph_call.go`, `server/management.go`, `server/server.go` | Call path and wiring through the resolver |
| `server/mcp/*`, `server/storage/*` | `auth_env` instead of a credential reference |
| `server/migrations/{sqlite,postgres}/001_initial.sql` | No credentials table; `mcp_servers.auth_env` |
| deleted: `server/credential/`, `server/provider/`, storage credential repo | |
| `ui/src/api/client.ts`, `ui/src/routes/{providers,dashboard,onboarding,mcp,agents}/**` | New providers shape, status page, `StageFields` |
| `scripts/dev.sh`, `scripts/dev-seed.mjs`, `e2e/**`, `tools/livecheck/**` | Env-based credential setup |
| `docs/**`, `config/*.yaml`, `deploy/**` | Docs and config |

---

### Task 0: Correct the spec where the code disagrees

Reading the code showed three details in the spec that are wrong or unneeded.

**Files:**
- Modify: `docs/superpowers/specs/2026-10-11-credentials-catalog-design.md`

- [ ] **Step 1: Apply these edits to the spec**
  1. In the seed table, delete the TTS model remarks (`model eleven_flash_v2_5; voice ...` becomes `voice 21m00Tcm4TlvDq8ikWAM`; `model sonic-3; voice ...` becomes `voice 694f9389-...`). An agent's TTS config has no `model` field (adapters use their defaults), so a TTS model list would be unused.
  2. In section 5 (MCP), delete the sentence about an `auth_missing` validation state. A server whose variable is unset fails validation through the existing failure path; the view exposes `auth_env_set` instead.
  3. In section 3 (What is deleted), add `server/provider` (the hard-coded definitions and the `xaiCapabilities` build-tag hook): the catalog replaces it, and adapter-factory presence (`registry.Supports`) already encodes the `xaispeech` tag.
- [ ] **Step 2: Commit** `docs: correct credentials spec (no TTS models, no auth_missing state, provider package deleted)`

---

### Task 1: Catalog model, strict loading, validation, packaged data

**Files:**
- Create: `server/catalog/catalog.go`, `server/catalog/catalog_test.go`, `server/catalog/data/{google,xai,elevenlabs,deepgram,cartesia,openai,nvidia}.yaml`

**Interfaces:**
- Produces:
  - `type Catalog struct{...}`; `func Load() (*Catalog, error)` (packaged files only; any error is fatal)
  - `func Parse(name string, data []byte) (*Provider, error)` (strict, validated)
  - `func (c *Catalog) Provider(id string) (*Provider, bool)`, `func (c *Catalog) Providers() []*Provider` (sorted by id)
  - `type Provider struct{ ID, Name string; Credentials []Kind; Services map[string]*Service }`
  - `type Kind struct{ ID, Label string; Fields map[string]Field; Assemble string }`, `type Field struct{ Env string; Optional bool }`
  - `type Service struct{ Credential, Needs []string; Models, Voices, Languages []Entry; Custom *bool }`, `func (s *Service) AllowsCustom() bool`
  - `type Entry struct{ ID, Label string; Default bool }`, `func (e []Entry) Default()` via `func DefaultEntry(es []Entry) (Entry, bool)`
  - consts `ServiceSTT = "stt"`, `ServiceLLM = "llm"`, `ServiceTTS = "tts"`

- [ ] **Step 1: Write the failing tests** (`catalog_test.go`)

```go
package catalog

import (
	"strings"
	"testing"
)

const goodXAI = `
id: xai
name: xAI
credentials:
  - id: key
    label: xAI API key
    fields:
      api_key: {env: XAI_API_KEY}
    assemble: token
services:
  llm:
    credential: [key]
    models:
      - {id: grok-2-1212, label: Grok 2, default: true}
  tts:
    credential: [key]
    voices:
      - {id: eve, label: Eve, default: true}
      - {id: ara, label: Ara}
`

func TestPackagedCatalogLoads(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"google", "xai", "elevenlabs", "deepgram", "cartesia", "openai", "nvidia"} {
		if _, ok := c.Provider(id); !ok {
			t.Errorf("packaged provider %q missing", id)
		}
	}
	g, _ := c.Provider("google")
	if got := g.Services[ServiceLLM].Credential; len(got) != 2 || got[0] != "api_key" {
		t.Fatalf("google llm must prefer the api key, got %v", got)
	}
	if got := g.Services[ServiceSTT].Credential; len(got) != 1 || got[0] != "service_account" {
		t.Fatalf("google stt needs the service account, got %v", got)
	}
}

func TestParseGood(t *testing.T) {
	p, err := Parse("xai.yaml", []byte(goodXAI))
	if err != nil {
		t.Fatal(err)
	}
	if d, ok := DefaultEntry(p.Services[ServiceTTS].Voices); !ok || d.ID != "eve" {
		t.Fatalf("default voice = %+v", d)
	}
	if !p.Services[ServiceTTS].AllowsCustom() {
		t.Fatal("custom entries are allowed unless the catalog says otherwise")
	}
}

func TestParseRejects(t *testing.T) {
	mutate := func(from, to string) string { return strings.Replace(goodXAI, from, to, 1) }
	cases := map[string]string{
		"unknown key":          mutate("name: xAI", "name: xAI\nbogus: 1"),
		"bad provider id":      mutate("id: xai", "id: XAI!"),
		"empty name":           mutate("name: xAI", "name: ''"),
		"unknown assembler":    mutate("assemble: token", "assemble: magic"),
		"bad env name":         mutate("XAI_API_KEY", "xai_api_key"),
		"unknown kind in svc":  mutate("credential: [key]\n    models", "credential: [nope]\n    models"),
		"unknown service":      mutate("  tts:", "  video:"),
		"two defaults":         mutate("{id: ara, label: Ara}", "{id: ara, label: Ara, default: true}"),
		"duplicate entry id":   mutate("{id: ara, label: Ara}", "{id: eve, label: Eve again}"),
		"empty entry id":       mutate("{id: ara, label: Ara}", "{id: '', label: Ara}"),
		"token with two fields": mutate("api_key: {env: XAI_API_KEY}", "api_key: {env: XAI_API_KEY}\n      other: {env: XAI_OTHER}"),
		"needs missing field":  mutate("credential: [key]\n    models", "credential: [key]\n    needs: [nope]\n    models"),
		"service without kind": mutate("credential: [key]\n    models", "credential: []\n    models"),
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse("x.yaml", []byte(doc)); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestParseCapsEntries(t *testing.T) {
	var b strings.Builder
	b.WriteString(strings.Replace(goodXAI, "      - {id: ara, label: Ara}\n", "", 1))
	for i := 0; i < maxEntries; i++ {
		b.WriteString("      - {id: v" + strings.Repeat("x", 1) + string(rune('a'+i%26)) + string(rune('a'+i/26%26)) + string(rune('a'+i/676)) + ", label: V}\n")
	}
	if _, err := Parse("x.yaml", []byte(b.String())); err == nil {
		t.Fatalf("more than %d entries accepted", maxEntries)
	}
}
```

- [ ] **Step 2: Run to verify failure** `cd server && go test ./catalog/ -v` → FAIL (`undefined: Load`).

- [ ] **Step 3: Implement `catalog.go`**

```go
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
func Parse(name string, data []byte) (*Provider, error) {
	if len(data) > MaxFileBytes {
		return nil, fmt.Errorf("%s: larger than %d bytes", name, MaxFileBytes)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var p Provider
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if err := p.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return &p, nil
}

func (p *Provider) validate() error {
	if !providerID.MatchString(p.ID) {
		return fmt.Errorf("provider id %q must match %s", p.ID, providerID)
	}
	if strings.TrimSpace(p.Name) == "" {
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
		if len(svc.Credential) == 0 {
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
```

- [ ] **Step 4: Write the seven data files** (`server/catalog/data/`). Values are the ones verified in this repo (spec, section 1):

`google.yaml`
```yaml
id: google
name: Google
credentials:
  - id: api_key
    label: Google API key
    fields:
      api_key: {env: GOOGLE_API_KEY}
    assemble: token
  - id: service_account
    label: Google service account
    fields:
      path: {env: GOOGLE_APPLICATION_CREDENTIALS}
    assemble: file_json
services:
  llm:
    credential: [api_key, service_account]
    models:
      - {id: gemini-2.5-flash, label: Gemini 2.5 Flash, default: true}
  stt:
    credential: [service_account]
    models:
      - {id: chirp_2, label: Chirp 2, default: true}
      - {id: latest_long, label: Latest long}
    languages:
      - {id: en-US, label: English (US), default: true}
  tts:
    credential: [service_account]
    voices:
      - {id: en-US-Chirp3-HD-Aoede, label: Chirp 3 HD Aoede (en-US), default: true}
```
`xai.yaml`
```yaml
id: xai
name: xAI
credentials:
  - id: key
    label: xAI API key
    fields:
      api_key: {env: XAI_API_KEY}
    assemble: token
services:
  llm:
    credential: [key]
    models:
      - {id: grok-2-1212, label: Grok 2, default: true}
  stt:
    credential: [key]
    models:
      - {id: grok-voice-transcribe-2.0, label: Grok voice transcribe 2.0, default: true}
    languages:
      - {id: en, label: English, default: true}
  tts:
    credential: [key]
    voices:
      - {id: eve, label: Eve, default: true}
      - {id: ara, label: Ara}
```
`elevenlabs.yaml`
```yaml
id: elevenlabs
name: ElevenLabs
credentials:
  - id: key
    label: ElevenLabs API key
    fields:
      api_key: {env: ELEVENLABS_API_KEY}
    assemble: token
services:
  stt:
    credential: [key]
    models:
      - {id: scribe_v2_realtime, label: Scribe v2 realtime, default: true}
    languages:
      - {id: en, label: English, default: true}
  tts:
    credential: [key]
    voices:
      - {id: 21m00Tcm4TlvDq8ikWAM, label: Rachel, default: true}
```
`deepgram.yaml`
```yaml
id: deepgram
name: Deepgram
credentials:
  - id: key
    label: Deepgram API key
    fields:
      api_key: {env: DEEPGRAM_API_KEY}
    assemble: token
services:
  stt:
    credential: [key]
    models:
      - {id: nova-3, label: Nova 3, default: true}
    languages:
      - {id: en, label: English, default: true}
```
`cartesia.yaml`
```yaml
id: cartesia
name: Cartesia
credentials:
  - id: key
    label: Cartesia API key
    fields:
      api_key: {env: CARTESIA_API_KEY}
    assemble: token
services:
  stt:
    credential: [key]
    models:
      - {id: ink-whisper, label: Ink Whisper, default: true}
    languages:
      - {id: en, label: English, default: true}
  tts:
    credential: [key]
    voices:
      - {id: 694f9389-aac1-45b6-b726-9d9369183238, label: Default voice, default: true}
```
`openai.yaml`
```yaml
id: openai
name: OpenAI-compatible
credentials:
  - id: key
    label: OpenAI-compatible API key
    fields:
      api_key: {env: OPENAI_API_KEY}
      base_url: {env: OPENAI_BASE_URL, optional: true}
    assemble: json
services:
  llm:
    credential: [key]
    models:
      - {id: gpt-4o-mini, label: GPT-4o mini, default: true}
```
`nvidia.yaml`
```yaml
id: nvidia
name: NVIDIA (NIM, Riva)
credentials:
  - id: key
    label: NVIDIA API key
    fields:
      api_key: {env: NVIDIA_API_KEY}
    assemble: token
  - id: riva
    label: NVIDIA API key with Riva function ids
    fields:
      api_key: {env: NVIDIA_API_KEY}
      function_id: {env: NVIDIA_STT_FUNCTION_ID, optional: true}
      tts_function_id: {env: NVIDIA_TTS_FUNCTION_ID, optional: true}
    assemble: json
services:
  llm:
    credential: [key]
    models:
      - {id: openai/gpt-oss-20b, label: GPT-OSS 20B, default: true}
  stt:
    credential: [riva]
    needs: [function_id]
    languages:
      - {id: en-US, label: English (US), default: true}
  tts:
    credential: [riva]
    needs: [tts_function_id]
    voices:
      - {id: Magpie-Multilingual.EN-US.Aria, label: Magpie Aria (en-US), default: true}
```

- [ ] **Step 5: Run** `cd server && go test -race ./catalog/ -v` → PASS (all four tests).
- [ ] **Step 6: Commit** `feat(catalog): provider catalog with strict loading, validation and packaged data`.

---

### Task 2: Operator overlay

**Files:**
- Create: `server/catalog/overlay.go`, `server/catalog/overlay_test.go`
- Modify: `server/config.go` (add `Providers.CatalogDir`), `server/testdata/effective_{development,production}.json` (regenerate), `server/server.go` (`NewServer` loads the catalog)

**Interfaces:**
- Produces: `func LoadWithOverlay(dir string, logf func(format string, args ...any)) (*Catalog, error)`; `Config.Providers.CatalogDir string \`yaml:"catalog_dir"\``; `Server.catalog *catalog.Catalog`.
- Consumes: `Load`, `Parse`, `MaxFileBytes` (Task 1).

Merge rules: for a provider id that exists, the overlay's `services.<s>.models|voices|languages` are merged (same id replaces the packaged entry, new ids are appended, a `default: true` in the overlay moves the default); `credentials`, `assemble`, a service's `credential`/`needs` and the provider's `name` come only from the packaged file (a differing value in the overlay is an error for that file). A provider id with no packaged file is refused with a logged reason (it needs an adapter). Any error skips that file.

- [ ] **Step 1: Failing tests** (`overlay_test.go`): write overlay files into `t.TempDir()` and assert, with a `logf` that records messages:
  1. an overlay adding voice `ara2` and replacing the label of `eve` yields both, packaged `ara` intact;
  2. an overlay with `default: true` on a new voice moves the default (still exactly one default);
  3. a file with an unknown top-level key, a file over 64 KiB, a file overriding `credentials`, a file for an unknown provider, and a file that is not YAML are each skipped with one log line, and `LoadWithOverlay` still returns the packaged catalog unchanged;
  4. a missing directory is an error (a typo in config must not pass silently); an empty `dir` means no overlay;
  5. non-`.yaml` files in the directory are ignored.
- [ ] **Step 2: Run, expect FAIL.**
- [ ] **Step 3: Implement** `overlay.go`. Core of the merge:

```go
func (c *Catalog) merge(o *Provider) error {
	base, ok := c.byID[o.ID]
	if !ok {
		return fmt.Errorf("provider %q has no packaged definition (a new provider needs an adapter in the build)", o.ID)
	}
	if len(o.Credentials) != 0 && !reflect.DeepEqual(o.Credentials, base.Credentials) {
		return errors.New("an overlay cannot change credentials or assemble")
	}
	next := *base
	next.Services = map[string]*Service{}
	for n, s := range base.Services {
		cp := *s
		cp.Models, cp.Voices, cp.Languages = slices.Clone(s.Models), slices.Clone(s.Voices), slices.Clone(s.Languages)
		next.Services[n] = &cp
	}
	for n, os := range o.Services {
		bs, ok := next.Services[n]
		if !ok {
			return fmt.Errorf("service %q is not declared by the packaged %s", n, o.ID)
		}
		if !slices.Equal(os.Credential, bs.Credential) && len(os.Credential) != 0 || !slices.Equal(os.Needs, bs.Needs) && len(os.Needs) != 0 {
			return errors.New("an overlay cannot change a service's credential or needs")
		}
		bs.Models, bs.Voices, bs.Languages = mergeEntries(bs.Models, os.Models), mergeEntries(bs.Voices, os.Voices), mergeEntries(bs.Languages, os.Languages)
		if os.Custom != nil {
			bs.Custom = os.Custom
		}
	}
	if err := next.validate(); err != nil {
		return err
	}
	c.byID[o.ID] = &next
	return nil
}
```
(`mergeEntries` replaces by id, appends new ones, and if an overlay entry has `Default` clears the base default.) The overlay file is parsed with a variant of `Parse` that skips the "services need credentials" rule for overlay files (an overlay service lists no `credential`), so `Parse` gets an unexported `parse(name, data, overlay bool)`; `Parse` calls it with `false`.
- [ ] **Step 4: Config and wiring.** Add to `server/config.go`:

```go
	// Providers: the provider catalog (server/catalog). CatalogDir is an optional directory of
	// overlay files that add or replace models, voices and languages; see docs/providers.md.
	Providers struct {
		CatalogDir string `yaml:"catalog_dir"`
	} `yaml:"providers"`
```
(check first: `grep -n "providers" server/config.go`; if a `Providers` struct already exists for per-provider infra settings, add `CatalogDir` to it instead of a new struct). In `NewServer`, after config validation: `cat, err := catalog.LoadWithOverlay(cfg.Providers.CatalogDir, slog.Warn-adapter)`; `return nil, err` on error; store `s.catalog = cat`.
- [ ] **Step 5: Regenerate goldens** `UPDATE_GOLDEN=1 go test ./server/ -run TestConfig_ShippedYAMLEffectiveGolden`; check the diff is only the new key.
- [ ] **Step 6: Run** `go test -race ./catalog/ ./server/ -run 'Overlay|Config' -v` → PASS. **Commit** `feat(catalog): operator overlay directory`.

---

### Task 3: Live resolver (detection, assemblers, secrets)

**Files:**
- Create: `server/catalog/resolver.go`, `server/catalog/resolver_test.go`

**Interfaces:**
- Produces:
  - `func NewResolver(c *Catalog, getenv func(string) string) *Resolver` (also `NewResolverWithFS(c, getenv, readFile func(path string, max int64) ([]byte, error))` for tests)
  - `type Status struct{ Declared, Detected bool; MissingEnv []string; Detail string }`
  - `func (r *Resolver) Status(provider, service string) Status`
  - `func (r *Resolver) Secret(provider, service string) ([]byte, error)` (a fresh buffer; the caller wipes it); error `*MissingError{Provider, Service string; Env []string}`; `ErrUnknown`
  - `func (r *Resolver) Env(provider string) []string` (every variable the provider's kinds read, sorted)
  - `func (r *Resolver) Detected(provider string) bool` (any kind detected)
  - `func (r *Resolver) Masked(provider string) string` (last 4 characters of a detected token for the status page; empty when none; never more than 4)
- Consumes: `Catalog`, `Provider`, `Kind`, `Service` (Task 1).

- [ ] **Step 1: Failing tests** (`resolver_test.go`), table-driven with `env := map[string]string{...}` and `NewResolver(cat, func(k string) string { return env[k] })`, a fake `readFile` backed by a map:
  1. `xai` with `XAI_API_KEY` → `Status(xai, llm)` detected; `Secret` returns the trimmed key; with the variable unset → `*MissingError` listing `XAI_API_KEY`; with `"  key  "` → trimmed; with `"a b"`, a value containing a newline, or a value starting with `$` → not detected.
  2. **Google** (Review Focus 6): only `GOOGLE_API_KEY` → llm detected (secret = key), stt and tts `MissingEnv == [GOOGLE_APPLICATION_CREDENTIALS]`; only the service-account file → llm resolves to the compact JSON, stt and tts detected; both → llm uses the API key, stt uses the file.
  3. **`file_json`** (Review Focus 7): unreadable path, non-JSON content, directory, content over 64 KiB → not detected, `Detail` mentions the variable and the problem class, `MissingEnv == [GOOGLE_APPLICATION_CREDENTIALS]`; valid pretty JSON → compact in the secret.
  4. **OpenAI json:** key only → `{"api_key":"k"}`; key plus base URL → both keys; base URL only → not detected.
  5. **NVIDIA:** `NVIDIA_API_KEY` only → llm detected (kind `key`, plain token), stt and tts not (`MissingEnv == [NVIDIA_STT_FUNCTION_ID]` and `[NVIDIA_TTS_FUNCTION_ID]`); with both function ids → stt/tts detected and the json holds `api_key`, `function_id`, `tts_function_id`.
  6. **Live** (Review Focus 2): `Secret` succeeds, the test deletes the variable, the next `Secret` fails; changing the value changes the result; nothing is cached.
  7. `Secret("nope", "llm")` and `Secret("xai", "video")` → `ErrUnknown`; a provider that does not declare the service (`deepgram`, `llm`) → `Declared == false`.
  8. `Env("nvidia")` lists all three variables sorted; `Masked("xai")` is exactly the last four characters.
- [ ] **Step 2: Run, expect FAIL.**
- [ ] **Step 3: Implement** `resolver.go`:

```go
package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

var ErrUnknown = errors.New("catalog: unknown provider or service")

type MissingError struct {
	Provider, Service string
	Env               []string
}

func (e *MissingError) Error() string {
	return fmt.Sprintf("%s %s is not configured: set %s", e.Provider, e.Service, strings.Join(e.Env, ", "))
}

type Status struct {
	Declared   bool
	Detected   bool
	MissingEnv []string
	Detail     string
}

type Resolver struct {
	cat      *Catalog
	getenv   func(string) string
	readFile func(path string, max int64) ([]byte, error)
}

func NewResolver(c *Catalog, getenv func(string) string) *Resolver {
	return NewResolverWithFS(c, getenv, readLimited)
}

func NewResolverWithFS(c *Catalog, getenv func(string) string, read func(string, int64) ([]byte, error)) *Resolver {
	return &Resolver{cat: c, getenv: getenv, readFile: read}
}

func readLimited(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.IsDir() {
		return nil, errors.New("is a directory")
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errors.New("file too large")
	}
	return b, nil
}

// values reads a kind's fields from the environment now. missing lists the
// variables that are required but unusable; detail says why for file fields.
func (r *Resolver) values(k *Kind) (vals map[string]string, missing []string, detail string) {
	vals = map[string]string{}
	ids := make([]string, 0, len(k.Fields))
	for id := range k.Fields {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		f := k.Fields[id]
		v := strings.TrimSpace(r.getenv(f.Env))
		if v == "" {
			if !f.Optional {
				missing = append(missing, f.Env)
			}
			continue
		}
		if k.Assemble == "file_json" {
			b, err := r.readFile(v, MaxFileBytes)
			var raw json.RawMessage
			if err != nil || json.Unmarshal(b, &raw) != nil {
				missing = append(missing, f.Env)
				detail = f.Env + " does not name a readable JSON file"
				continue
			}
			compact, _ := json.Marshal(raw)
			vals[id] = string(compact)
			continue
		}
		if strings.HasPrefix(v, "$") || strings.IndexFunc(v, func(c rune) bool { return c <= ' ' || c == 0x7f }) >= 0 {
			if !f.Optional {
				missing = append(missing, f.Env)
			}
			detail = f.Env + " is not a single token"
			continue
		}
		vals[id] = v
	}
	return vals, missing, detail
}

// satisfies reports whether kind k can serve svc, and what is missing if not.
func (r *Resolver) satisfies(k *Kind, svc *Service) (map[string]string, []string, string, bool) {
	vals, missing, detail := r.values(k)
	for _, need := range svc.Needs {
		f, has := k.Fields[need]
		if !has {
			return nil, nil, "", false
		}
		if _, got := vals[need]; !got {
			missing = append(missing, f.Env)
		}
	}
	return vals, uniq(missing), detail, len(missing) == 0
}

func (r *Resolver) service(provider, service string) (*Provider, *Service, bool) {
	p, ok := r.cat.Provider(provider)
	if !ok {
		return nil, nil, false
	}
	s, ok := p.Services[service]
	return p, s, ok
}

func (r *Resolver) Status(provider, service string) Status {
	p, svc, ok := r.service(provider, service)
	if !ok {
		return Status{}
	}
	var firstMissing []string
	var firstDetail string
	for i, kid := range svc.Credential {
		k, _ := p.Kind(kid)
		_, missing, detail, ok := r.satisfies(k, svc)
		if ok {
			return Status{Declared: true, Detected: true}
		}
		if i == 0 {
			firstMissing, firstDetail = missing, detail
		}
	}
	return Status{Declared: true, MissingEnv: firstMissing, Detail: firstDetail}
}

func (r *Resolver) Secret(provider, service string) ([]byte, error) {
	p, svc, ok := r.service(provider, service)
	if !ok {
		return nil, ErrUnknown
	}
	var firstMissing []string
	for i, kid := range svc.Credential {
		k, _ := p.Kind(kid)
		vals, missing, _, ok := r.satisfies(k, svc)
		if !ok {
			if i == 0 {
				firstMissing = missing
			}
			continue
		}
		return assemble(k, vals)
	}
	return nil, &MissingError{Provider: provider, Service: service, Env: firstMissing}
}

func assemble(k *Kind, vals map[string]string) ([]byte, error) {
	switch k.Assemble {
	case "token", "file_json":
		for _, v := range vals { // exactly one field (validated)
			return []byte(v), nil
		}
		return nil, errors.New("catalog: empty secret")
	case "json":
		return json.Marshal(vals) // keys sorted; only present fields
	}
	return nil, fmt.Errorf("catalog: unknown assembler %q", k.Assemble)
}

func uniq(in []string) []string {
	sort.Strings(in)
	out := in[:0]
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}
```
Also implement `Env(provider)`, `Detected(provider)` (any declared service's preferred-or-alternative kind currently detected) and `Masked(provider)` (last four runes of the first detected `token` value, else "").
- [ ] **Step 4: Run** `go test -race ./catalog/ -v` → PASS.
- [ ] **Step 5: Adapter-parse tests** (in package `server`, new file `server/catalog_adapters_test.go`, so it can import the adapters): for each seed provider build the assembled secret from a fake environment and pass it to the adapter's own parser, asserting no error and the expected fields: `google.CredentialMaterialFromSecret` (API key and a service-account JSON; reuse the valid service-account fixture the google package's tests use: `grep -rn "private_key" providers/google/*_test.go`), `openAICredential` (key, and key plus base URL), and the NVIDIA credential parser in `server/nvidia.go` (`grep -n "func .*[Cc]redential" server/nvidia.go`) with function ids.
- [ ] **Step 6: Commit** `feat(catalog): live resolver with token, json and file_json assemblers`.

---

### Task 4: Availability and the new providers API

**Files:**
- Create: `server/providers.go`, `server/providers_test.go`
- Rewrite: `server/api/providers.go` (GET only; the credential routes stay until Task 7)
- Modify: `server/server.go` (`Server.env`, `Server.resolver`, `Server.availability`), `server/management.go` (pass the new handler), `server/api_boundary_test.go` (`apiTestServer` sets a test environment)

**Interfaces:**
- Produces:
  - in `server/providers.go`: `type providerAvailability struct{ cat *catalog.Catalog; res *catalog.Resolver; supports func(provider, service string) bool }` with `Check(provider, service string) agent.Unavailable` (defined in Task 5; this task defines the method and a local `unavailable` struct, and Task 5 swaps the return type to `agent.Unavailable`: see the note in Step 3);
  - `Server.env func(string) string` (default `os.Getenv`), `Server.resolver *catalog.Resolver` built on `func(k string) string { return s.env(k) }`;
  - `api.ProvidersHandler{Catalog *catalog.Catalog, Resolver *catalog.Resolver, Supports func(provider, service string) bool}` serving `GET /api/v1/providers`.
- Response (spec, section 4): per provider `id`, `display_name`, `capabilities` (declared services that are available), `detected`, `env` (variable names), `services{stt|llm|tts: {available, reason?, missing_env?, models?, voices?, languages?, custom}}`.

- [ ] **Step 1: Failing tests** (`providers_test.go` in package `server`, using `s.env = mapEnv(...)`):
  1. no keys → every provider `detected:false`, every service `available:false` with `reason:"missing_env"` and the variable names, `env` lists the variables;
  2. `XAI_API_KEY` set and the xAI speech factories registered → `xai.detected`, llm/stt/tts available, `tts.voices` has `eve` as default;
  3. a build without xAI speech (`delete(s.registry.factories, factoryKey("xai","stt"))`) → `xai.stt` is `available:false, reason:"not_in_build"` while llm stays available;
  4. the response body never contains the environment value (assert on the raw bytes);
  5. an undetected provider is still listed with `detected:false`;
  6. `Check("xai","tts")` returns reason `""` when available, `missing_credential` plus `MissingEnv` when not, `unknown_provider` for `Check("nope","llm")`, `unsupported_provider` for `Check("deepgram","llm")`.
- [ ] **Step 2: Run, expect FAIL.**
- [ ] **Step 3: Implement.** `Check` order: provider not in the catalog → `unknown_provider`; service not declared, or `!supports(provider, service)` → `unsupported_provider` (`not_in_build` is the API's word for the second case; `Check` uses one code for both because readiness only needs to refuse); `res.Status(...).Detected` false → `missing_credential` with `MissingEnv`. Task 5 introduces `agent.Unavailable`; to keep this task compiling define `type unavailable struct{ Reason string; MissingEnv []string }` here and change it in Task 5, Step 3 (one rename).
  `api.ProvidersHandler.ServeHTTP`: `GET /api/v1/providers` only (405 otherwise); builds the response from `Catalog.Providers()`, and for each declared service computes `available := Supports(p.ID, svc) && Resolver.Status(p.ID, svc).Detected`, `reason` = `not_in_build` when `!Supports`, else `missing_env`. The raw value of any variable is never read by this handler; `Masked` is not used here (status page shows only detection).
  Wire it in `management.go` in place of the current `SetProviderManagementAPI(&api.ProviderManagement{...})` for the listing route; keep the credential routes in the old handler under their own paths until Task 7 deletes them (`/api/v1/providers/credentials...` continue to hit the old handler).
  In `NewServer`: `s.env = os.Getenv`; `s.resolver = catalog.NewResolver(s.catalog, func(k string) string { return s.env(k) })`; `s.availability = providerAvailability{...}` with `supports: s.registry.Supports`.
  In `apiTestServer`: set `s.env` to a helper `testProviderEnv(t)` returning a map-backed getenv with `GOOGLE_API_KEY=test-google-api-key`, `GOOGLE_APPLICATION_CREDENTIALS=<temp file containing {"type":"service_account"}>`, `XAI_API_KEY=test-xai-key`.
- [ ] **Step 4: Run** `go test -race ./server/ -run 'Provider|Availability' -v` → PASS. **Step 5: Commit** `feat(server): provider availability and the catalog-backed providers API`.

---

### Task 5: Agent package without credentials

**Files:**
- Modify: `server/agent/config.go`, `server/agent/fallback.go`, `server/agent/graph.go`, `server/agent/service.go`, `server/agent/*_test.go`
- Modify: `server/providers.go` (return `agent.Unavailable`), `server/management.go` (`agent.NewService(store, s.availability)`)

**Interfaces:**
- Produces:
  - `type Availability interface{ Check(provider, service string) Unavailable }`, `type Unavailable struct{ Reason string; MissingEnv []string }`
  - `func AttachmentProblems(c Config, av Availability) (reasons, missingEnv []string)`
  - `func FallbackProblems(c Config, av Availability) (reasons, missingEnv []string)`
  - `func (g Graph) Problems(av Availability) (reasons, missingEnv []string)`
  - `func NewService(store *storage.Store, av Availability) *Service`
  - `Readiness.MissingEnv []string \`json:"missing_env,omitempty"\``
  - `Stage{Name, Provider string}`, `Candidate` without `CredentialID`
- Removed: `CredentialID` on `STTConfig`, `LLMConfig`, `TTSConfig`, the three fallback structs, `Stage`, `Candidate`; `ResolveCredential`; the `creds []storage.Credential` parameters everywhere in this package.

- [ ] **Step 1: Failing tests.** In `server/agent` add `availability_test.go` with a map-backed fake:

```go
type fakeAvail map[string]Unavailable // key "provider/service"; absent = available

func (f fakeAvail) Check(provider, service string) Unavailable { return f[provider+"/"+service] }

func ready() fakeAvail { return fakeAvail{} }
```
and tests: (a) `AttachmentProblems` on a complete config with `ready()` is empty; (b) with `fakeAvail{"google/stt": {Reason: "missing_credential", MissingEnv: []string{"GOOGLE_APPLICATION_CREDENTIALS"}}}` it returns `["missing_credential_stt"]` and the env list; (c) two stages missing the same variable list it once; (d) `FallbackProblems` reasons look like `missing_credential_llm_fallback_1`; (e) **Review Focus 5:** `DecodeConfig` of a config containing `"credential_id"` on a stage, on a fallback, and on a graph node's stage returns `ErrInvalidConfig` whose message contains `credential_id`; (f) a `Graph` with a non-entry node borrowing the entry's STT (existing behaviour) is checked through `Problems(av)` with the node id prefix.
  Update the existing agent tests: delete credential setup from `testService`/`seedCredentials`/`validConfig` (the helper `testCredentialID` goes away; configs lose `"credential_id"`), build the service with `NewService(store, ready())`; tests that asserted credential-specific reasons are rewritten to the equivalent `fakeAvail` case or deleted when they only tested credential storage.
- [ ] **Step 2: Run** `cd server && go test ./agent/ 2>&1 | head` → compile failures listing the removed symbols.
- [ ] **Step 3: Implement.** Delete the `CredentialID` fields and `ResolveCredential`; `Config.Stages()` returns `[]Stage{{Name: "stt", Provider: c.STT.Provider}, ...}`; replace the problem functions:

```go
func AttachmentProblems(c Config, av Availability) (reasons, missingEnv []string) {
	if m := c.Missing(); len(m) > 0 {
		return m, nil
	}
	for _, st := range c.Stages() {
		if u := av.Check(st.Provider, st.Name); u.Reason != "" {
			reasons = append(reasons, u.Reason+"_"+st.Name)
			missingEnv = append(missingEnv, u.MissingEnv...)
		}
	}
	return reasons, uniq(missingEnv)
}
```
(`FallbackProblems` is the same loop over `c.Chain(stage)[1:]` with reason `u.Reason + "_" + stage + "_fallback_" + strconv.Itoa(cand.Index)`; `Graph.Problems` calls both per node and prefixes non-entry reasons with `node.ID + ":"`.) `Service` gets `av Availability` instead of `providers *provider.Registry` and `store.Credentials().List` calls disappear (`agentReadiness(ctx, v, enabled, archived)` returns `Readiness` with `MissingEnv` filled from `AttachmentProblems`). `attachedConfig` returns `ErrNotAttached` wrapping the reasons as today. Rename in `server/providers.go` the local `unavailable` type to `agent.Unavailable`.
- [ ] **Step 4: Run** `go test -race ./agent/` → PASS (the server package does not compile until Task 6; that is expected: do not run it yet).
- [ ] **Step 5: Commit** `feat(agent): stages name no credential; readiness reports missing env`.

---

### Task 6: Call path through the resolver

**Files:**
- Modify: `server/call.go`, `server/graph_call.go`, `server/server.go`, `server/management.go`, `server/runtime/mcp.go` (only if it references removed agent API), server test helpers (`server/management_test.go` `readyTestAgent`, `server/agent_call_test.go`, `server/graph_*test.go`, `server/api_*test.go`)

**Interfaces:**
- Consumes: `Server.resolver` (Task 4), `agent.AttachmentProblems` (Task 5).
- Produces: `resolveStageSecrets(ctx, cfg, names ...string)` without the `creds` parameter; `resolveFallbacks(ctx, rs, stage, cfg, skipped, build)` likewise.

- [ ] **Step 1: Failing tests** (extend `server/agent_call_test.go` and `server/graph_e2e_test.go`'s unit-level siblings): (a) `TestStageSecretsComeFromTheEnvironmentAtCallTime`: set `s.env` so `GOOGLE_API_KEY=k1`, resolve → secret `k1`; change the map to `k2`, resolve again → `k2` (live); remove it → error naming `GOOGLE_API_KEY`; (b) `TestCallRefusedWhenTheVariableIsGone` (Review Focus 2): a ready agent, remove the variable, the WebSocket call is refused (existing refusal path used by "disabled agent" test) and no session row is created; (c) `TestGraphNodeActivationRereadsTheEnvironment`: a graph whose second node is on another provider; the second node's secret is read when it is activated (change the variable between call start and the handoff and assert the new value reaches the factory spec); (d) failover candidates resolve by provider (`xai` fallback of a `google` primary) and a candidate whose variable is unset is skipped and counted (existing counter), not fatal.
- [ ] **Step 2: Implement.** In `call.go` replace the three credential-touching spots (`callSpec`, `nodeSpec`, `resolveFallbacks`) so that:

```go
func (s *Server) resolveStageSecrets(ctx context.Context, cfg agent.Config, names ...string) (map[string][]byte, error) {
	secrets := map[string][]byte{}
	ok := false
	defer func() {
		if !ok {
			for _, b := range secrets {
				clear(b)
			}
		}
	}()
	for _, stage := range cfg.Stages() {
		if !slices.Contains(names, stage.Name) {
			continue
		}
		if !s.registry.Supports(stage.Provider, stage.Name) {
			return nil, fmt.Errorf("%s provider %q is not available on this server", stage.Name, stage.Provider)
		}
		secret, err := s.resolver.Secret(stage.Provider, stage.Name)
		if err != nil {
			return nil, fmt.Errorf("agent not callable: %s credential: %w", stage.Name, err)
		}
		secrets[stage.Name] = secret
	}
	ok = true
	return secrets, nil
}
```
(`*catalog.MissingError` already names the variables and never a value.) In `resolveFallbacks` replace the `ResolveCredential` + `ProviderSecret` pair by `secret, err := s.resolver.Secret(cand.Provider, stage)`; on error `skip("credential unavailable")` as before. Delete the `creds, err := s.store.Credentials().List(ctx)` blocks. In `resolveCall` replace `s.credentials == nil` in the guard by `s.resolver == nil`. In `management.go`: `agent.NewService(store, s.availability)`; keep `s.credentials` until Task 7 deletes it.
- [ ] **Step 3: Test helpers.** `readyTestAgent(t, s)` no longer creates a credential: it builds the config without `credential_id` using providers the test environment (Task 4) supports, and asserts readiness. Mechanical rules for the remaining fallout, applied by running `cd server && go vet ./... 2>&1 | head -50` until clean:
  | Old | New |
  |---|---|
  | `s.credentials.Create(ctx, credential.CreateInput{...})` + `"credential_id":"CRED"` in a config | delete both; configs name only provider/model/voice |
  | `seedCredentials`, `testCredentialID`, `storage.Credential{...}` fixtures | delete |
  | test expecting `invalid_credential_*` or `missing_credential_*` from a deleted credential | set/unset the variable in `s.env` and assert `missing_credential_*` plus `missing_env` |
  | a test of credential encryption or rotation | belongs to the deleted package: delete with it (Task 7) |
- [ ] **Step 4: Run** `go test -race ./server/ ./server/agent/ ./server/runtime/ -count=1` → PASS.
- [ ] **Step 5: Commit** `feat(server): the call path reads provider secrets from the environment`.

---

### Task 7: Delete the credential store

**Files:**
- Delete: `server/credential/`, `server/provider/`, `server/storage/credential_repository_test.go`, the credential repository in `server/storage/repositories.go` (`CredentialRepository`, `Credential`, `Credentials()`), credential half of `server/api/providers.go` and its tests
- Modify: `server/migrations/{sqlite,postgres}/001_initial.sql`, `server/management.go`, `server/server.go`, `server/config.go`, `server/config_defaults.go`, `config/{development,production}.yaml`, `server/testdata/effective_*.json`, `server/api_routes.go`

- [ ] **Step 1: Failing tests first** (they must fail on the current tree):
  1. `server/storage/storage_test.go`: `TestSchemaHasNoCredentialTable` opens a fresh store on both dialects (postgres through the existing `TEST_POSTGRES_DSN` guard) and asserts `SELECT 1 FROM provider_credentials` errors, and that `mcp_servers` has an `auth_env` column and no `auth_credential_id`;
  2. `server/api_provider_boundary_test.go`: `TestCredentialRoutesAreGone` asserts `GET/POST /api/v1/providers/credentials` and `/api/v1/providers/credentials/x` return 404 with the management key;
  3. `server/management_test.go`: `TestManagementStartsWithoutAKeyring` sets neither `GOVOICE_CREDENTIAL_KEYS` nor a dev key file and `EnableManagement` still succeeds (dev profile), and no `credential.key` is created in the dev dir.
- [ ] **Step 2: Edit the initial migrations in place** (both dialects): remove the `CREATE TABLE provider_credentials ...` line; in `mcp_servers` replace `auth_credential_id TEXT REFERENCES provider_credentials(id) ON DELETE RESTRICT` with `auth_env TEXT NOT NULL DEFAULT ''`. Remove any later reference: `grep -rn "provider_credentials\|auth_credential_id" server/migrations server/storage` must return nothing.
- [ ] **Step 3: Delete and unwire.** `git rm -r server/credential server/provider`; remove `Credentials()` and its repository from `storage`; in `management.go` delete `parseKeyring`, `loadOrCreateDevKey`, `developmentStore`'s key handling (keep its sqlite dev DSN creation), the `credential.NewService` block and `s.credentials`; in `server.go` delete the `credentials *credential.Service` field and the `credential.ErrDecrypt` case in the error mapper; in `config.go` delete `Credentials struct { KeyringEnv ... }` and its defaults/validation; in `api_routes.go` stop mounting the credential routes (the providers listing route from Task 4 stays); in `config/*.yaml` delete the `credentials:` section.
- [ ] **Step 4: Regenerate goldens** `UPDATE_GOLDEN=1 go test ./server/ -run TestConfig_ShippedYAMLEffectiveGolden`; the diff must be only the removed `Credentials` block.
- [ ] **Step 5: Run** `cd server && go build ./... && go vet ./... && go test -race -count=1 ./...` → PASS. `grep -rn "GOVOICE_CREDENTIAL_KEYS" . --include='*.go'` returns nothing.
- [ ] **Step 6: Commit** `refactor: delete the credential store, keyring, credential API and provider package`.

---

### Task 8: MCP bearer tokens from `MCP_TOKEN_*` variables

**Files:**
- Modify: `server/mcp/service.go`, `server/mcp/http.go`, `server/mcp/*_test.go`, `server/storage/repositories.go` (`MCPServer.AuthEnv`), `server/agent/service.go` (`MCPToolSnapshot.AuthEnv`), `server/runtime/mcp.go`, `server/management.go`, `docs/api/mcp.md`, `docs/security/mcp.md`
- Create: `server/mcp/envtokens.go`, `server/mcp/envtokens_test.go`

**Interfaces:**
- Produces: `mcp.ValidAuthEnv(name string) bool`; `type TokenSource interface{ Token(name string) (string, bool) }` replacing `CredentialResolver`; `mcp.EnvTokens(getenv func(string) string) TokenSource`; `ServerConfig.AuthEnv string \`json:"auth_env,omitempty"\``; `ServerView.AuthEnv string`, `ServerView.AuthEnvSet bool \`json:"auth_env_set"\``; `storage.MCPServer.AuthEnv`.

- [ ] **Step 1: Failing tests** (`envtokens_test.go`, Review Focus 3):

```go
func TestValidAuthEnv(t *testing.T) {
	good := []string{"MCP_TOKEN_CRM", "MCP_TOKEN_A", "MCP_TOKEN_" + strings.Repeat("X", 48)}
	bad := []string{"", "TOKEN_SECRET", "GOVOICE_MANAGEMENT_API_KEY", "XAI_API_KEY", "mcp_token_crm",
		"MCP_TOKEN_", "MCP_TOKEN_A B", "MCP_TOKEN_a", "MCP_TOKEN_" + strings.Repeat("X", 49), "MCP_TOKEN_A\n", "XMCP_TOKEN_A"}
	for _, n := range good {
		if !ValidAuthEnv(n) {
			t.Errorf("%q rejected", n)
		}
	}
	for _, n := range bad {
		if ValidAuthEnv(n) {
			t.Errorf("%q accepted", n)
		}
	}
}

func TestEnvTokensOnlyServesPrefixedSingleTokens(t *testing.T) {
	env := map[string]string{"MCP_TOKEN_CRM": " tok ", "TOKEN_SECRET": "must-not-leak", "MCP_TOKEN_BAD": "a b"}
	src := EnvTokens(func(k string) string { return env[k] })
	if v, ok := src.Token("MCP_TOKEN_CRM"); !ok || v != "tok" {
		t.Fatalf("got %q %v", v, ok)
	}
	for _, n := range []string{"TOKEN_SECRET", "MCP_TOKEN_BAD", "MCP_TOKEN_MISSING"} {
		if _, ok := src.Token(n); ok {
			t.Errorf("%s served", n)
		}
	}
}
```
plus service tests: creating a server with `auth_env: "TOKEN_SECRET"` fails with `invalid MCP auth_env`; a server with `auth_env` unset in the environment validates as failed (existing failure path) and its view has `auth_env_set:false`; with the variable set the bearer token reaches the fake remote exactly once and never appears in `ServerView` JSON; the old `credential_id` field in a create request is rejected (strict decode).
- [ ] **Step 2: Run, expect FAIL.**
- [ ] **Step 3: Implement.**

```go
var authEnvRe = regexp.MustCompile(`^MCP_TOKEN_[A-Z0-9_]{1,48}$`)

func ValidAuthEnv(name string) bool { return authEnvRe.MatchString(name) }

type TokenSource interface{ Token(name string) (string, bool) }

type envTokens struct{ getenv func(string) string }

func EnvTokens(getenv func(string) string) TokenSource { return envTokens{getenv} }

func (e envTokens) Token(name string) (string, bool) {
	if !ValidAuthEnv(name) {
		return "", false // never read a variable outside the prefix, even if asked
	}
	v := strings.TrimSpace(e.getenv(name))
	if v == "" || strings.IndexFunc(v, func(c rune) bool { return c <= ' ' || c == 0x7f }) >= 0 {
		return "", false
	}
	return v, true
}
```
`Service.credential(ctx, m)` becomes `token(m)` using `s.tokens.Token(m.AuthEnv)`, returning `"MCP authentication unavailable"` when unset; `CreateServer` validates `cfg.AuthEnv` (empty allowed) with `ValidAuthEnv`, replacing the `uuid.Parse` credential check; the endpoint-query error text changes from "use a credential reference" to "use auth_env"; the handler view computes `AuthEnvSet` from `TokenSource`. `ServiceConfig.Credentials` becomes `Tokens TokenSource`; `management.go` passes `mcp.EnvTokens(func(k string) string { return s.env(k) })`. Rename `CapturedTool.credentialID`/`CredentialID()` and `MCPToolSnapshot.CredentialID` to `AuthEnv` (a name, safe to record).
- [ ] **Step 4: Run** `go test -race ./mcp/ ./agent/ ./runtime/ ./ -count=1` → PASS. **Step 5: Docs:** `docs/api/mcp.md` and `docs/security/mcp.md` describe `auth_env`, the prefix rule and why. **Step 6: Commit** `feat(mcp): bearer tokens come from MCP_TOKEN_ environment variables`.

---

### Task 9: UI API client, provider status page, dashboard, onboarding

**Files:**
- Modify: `ui/src/api/client.ts` and `client.test.ts`, `ui/src/routes/providers/ProvidersView.tsx` and test, `ui/src/routes/dashboard/DashboardView.tsx` and test, `ui/src/routes/onboarding/OnboardingView.tsx` and test, `ui/src/app/*` if the nav label changes

**Interfaces:**
- Produces in `client.ts`:

```ts
export const serviceStatusSchema = z.object({
  available: z.boolean(),
  reason: z.enum(["missing_env", "not_in_build"]).optional(),
  missing_env: z.array(z.string()).optional(),
  models: z.array(catalogEntrySchema).optional(),
  voices: z.array(catalogEntrySchema).optional(),
  languages: z.array(catalogEntrySchema).optional(),
  custom: z.boolean().optional().default(true),
});
export const providerStatusSchema = z.object({
  id: z.string(), display_name: z.string(), capabilities: z.array(z.string()),
  detected: z.boolean(), env: z.array(z.string()),
  services: z.record(z.enum(["stt", "llm", "tts"]), serviceStatusSchema),
});
export type ProviderStatus = z.infer<typeof providerStatusSchema>;
// ApiClient.getProviders(): Promise<ProviderStatus[]>
```
(`catalogEntrySchema = z.object({id: z.string(), label: z.string(), default: z.boolean().optional()})`.) Removed: `CredentialDTO`, `getCredentials`, `createCredential`, `updateCredential`, `deleteCredential`, `rotateCredential`, `validateCredential`, the `credential_id` fields in `agentConfigV2Schema` and the MCP DTOs; `ProviderDefinition` becomes `ProviderStatus`.

- [ ] **Step 1: Failing tests.** `client.test.ts`: parsing a sample providers response (one detected, one not, one with `not_in_build`); no credential methods exist on the client (`expect("createCredential" in client).toBe(false)`). `ProvidersView.test.tsx`: renders a row per provider with the detected badge, lists the services each unlocks, shows the missing variables for undetected ones (`XAI_API_KEY`), has no inputs or buttons that change anything, and never renders a secret; the empty state (nothing detected) names the variables to set. `DashboardView.test.tsx`: the provider card says `N / M providers detected`. `OnboardingView.test.tsx`: the steps no longer mention adding a credential; they say which variables to set.
- [ ] **Step 2: Run** `cd ui && npx vitest run src/api src/routes/providers src/routes/dashboard src/routes/onboarding` → FAIL.
- [ ] **Step 3: Implement** the schema changes above; rewrite `ProvidersView` as a read-only table (provider, status badge, services, "set X to enable" for missing ones); update the dashboard counter to `providers.filter(p => p.detected).length`; update the onboarding copy and its gating (it checks "any provider detected").
- [ ] **Step 4: Run** the same vitest command, then `npx tsc --noEmit` → PASS (type errors in `AgentsView`, `Inspector` and `MCPView` that still use credentials are expected here and fixed in Task 10: run tsc at the end of Task 10, not now).
- [ ] **Step 5: Commit** `feat(ui): read-only provider status page driven by the catalog`.

---

### Task 10: UI `StageFields`, agent form, designer, MCP

**Files:**
- Create: `ui/src/routes/agents/StageFields.tsx`, `StageFields.test.tsx`
- Modify: `ui/src/routes/agents/AgentsView.tsx` (+ test), `ui/src/routes/agents/graph/{Inspector,GraphDesigner,model}.ts*` (+ tests), `ui/src/routes/mcp/MCPView.tsx` (+ test), `ui/src/routes/agents/{AgentVoiceTest,graph/LiveGraph}.test.tsx` fixtures

**Interfaces:**
- Produces:

```tsx
export type StageValue = { provider: string; model?: string; voice?: string; language?: string };
export type StageKind = "stt" | "llm" | "tts";
export function StageFields(props: {
  kind: StageKind;
  providers: ProviderStatus[];          // from getProviders()
  value: StageValue;
  onChange: (next: StageValue) => void;
  idPrefix: string;                      // unique ids per node/form
}): JSX.Element;
```
Behaviour: provider `<select>` lists only providers whose `services[kind].available`; choosing one sets `model`/`voice`/`language` to the catalog defaults (when present); model (STT, LLM) or voice (TTS) is a `<select>` of catalog entries whose last option is **Other…** (hidden when `custom: false`) revealing a text input; a stored value the catalog does not list is shown as `Other…` with its value; STT shows a language `<select>` when the catalog lists languages; if nothing is available the component renders a note naming the variables from `providers` (`missing_env`). Accessible names are `STT provider`, `STT model`, `STT language`, `LLM provider`, `LLM model`, `TTS provider` and `Voice` (the old agent form's `TTS voice` label becomes `Voice`, matching the designer; Task 11 updates the paid suite); there are no credential controls.

- [ ] **Step 1: Failing tests** (`StageFields.test.tsx`): (a) only available providers are options; (b) choosing a provider fills the catalog default model/voice/language; (c) the model select's last option is `Other…`, selecting it shows a text box whose value flows to `onChange`; (d) a value not in the catalog renders as `Other…` with the text; (e) `custom: false` hides `Other…`; (f) nothing available → a note listing `XAI_API_KEY` and the other variables; (g) language select appears only when the catalog lists languages. Update `model.test.ts`: `validate` no longer requires credentials (provider and model checks only; entry node still needs STT provider), `addNode` copies the entry's llm/tts provider and model without a credential, and `toPayload` output has no `credential_id`. `AgentsView.test.tsx` / `GraphDesigner.test.tsx`: fixtures lose credentials, the credential selects are gone, a create posts a config without `credential_id`. `MCPView.test.tsx`: the credential select becomes an `auth_env` text field with the `MCP_TOKEN_` prefix hint and a "variable is set" badge from `auth_env_set`; creating with `auth_env: "TOKEN_SECRET"` is blocked client-side with the prefix message.
- [ ] **Step 2: Run, expect FAIL.** **Step 3: Implement** `StageFields.tsx` and replace the stage blocks in `AgentForm` (`AgentsView.tsx`, ~lines 200 to 430: `providerSelect`, `credentialSelect`, the three `fieldset` blocks) and in `Inspector.tsx` (`stageFields`) with `<StageFields .../>`. The queries for `credentials` in `AgentsView`, `GraphDesigner` callers and `MCPView` are replaced by `getProviders()`. Remove `credential_id` from `AgentConfigV2` building (`buildAgentConfig`), from `model.ts` (`validate`, `addNode`) and from `agentConfigV2Schema`.
- [ ] **Step 4: Run** `npx vitest run` (all) and `npx tsc --noEmit` and `npm run build` → PASS.
- [ ] **Step 5: Commit** `feat(ui): shared StageFields with catalog dropdowns; agents and MCP lose credentials`.

---

### Task 11: Harnesses and dev flow

**Files:**
- Modify: `scripts/dev.sh`, `scripts/dev-seed.mjs`, `examples/.env.example`, `e2e/global-setup-graph.ts`, `e2e/global-setup.ts`, `e2e/tests-graph/10-agent-graph.spec.ts`, `e2e/tests/10-full-stack.spec.ts`, `e2e/lib/stack.ts`, `server/graph_e2e_test.go`, `server/e2e/scenarios_test.go`, `tools/livecheck/{agent,creds,server,run,main,profiles,scenarios}.go` and tests

- [ ] **Step 1: Failing tests / checks first.**
  1. `tools/livecheck/creds_test.go` (new): for every vendor in `credSpecs`, the set of environment variable names it requires equals the catalog's required variables for the provider's kinds (`catalog.Load()` imported from the `server` module via the workspace; if the module boundary blocks the import, copy the vendor table into a golden `testdata/catalog_envs.json` generated by a `go test -run TestWriteGolden -update` and compare both sides to it): this is the drift guard from the spec.
  2. `server/graph_e2e_test.go`: the rig stops creating a credential; `newGraphRigE2E` sets `s.env` and `graphConfigJSON` drops `credential_id`; all existing assertions stay.
  3. `e2e/tests-graph`: the spec stops posting to `/providers/credentials` and instead reads `/api/v1/providers`, asserts the provider it needs is `detected` and that **only** detected providers appear in the designer's provider dropdown (set `GOOGLE_API_KEY`/`GOOGLE_APPLICATION_CREDENTIALS` placeholders for the fake stack; xAI for the live mode); add a test that starts with a provider missing and expects the empty-state note.
- [ ] **Step 2: Implement.**
  - `scripts/dev.sh` loads `examples/.env` and exports the provider variables to the server process (`GOOGLE_API_KEY`, `GOOGLE_APPLICATION_CREDENTIALS` resolved to an absolute path, `XAI_API_KEY`, and the other catalog variables when present); it no longer creates a credential key.
  - `scripts/dev-seed.mjs` stops creating credentials and builds the "Dev Assistant" config from `GET /api/v1/providers` (first provider with each service available, catalog defaults).
  - `e2e/global-setup-graph.ts` passes provider variables to the server child: fake mode sets `GOOGLE_API_KEY=fake-google-key` and `GOOGLE_APPLICATION_CREDENTIALS=<tmp>/sa.json` (a file containing `{"type":"service_account"}`); live mode passes only `XAI_API_KEY` (still filtered: the `start()` helper's `GOOGLE_|XAI_|E2E_` strip stays, the explicit overrides are added after it).
  - **Selectors.** Model and voice are now dropdowns: in `e2e/tests-graph/10-agent-graph.spec.ts` replace `fill("chirp")`/`fill(LLM_MODEL)`/`fill("frontvoice")` by `selectOption` of the catalog default where it exists (live xAI: the defaults), and by `selectOption({ label: "Other…" })` followed by `fill` for values the catalog does not list (the fake stack's `fake-llm`, `frontvoice`, `salesvoice`), which also exercises the escape hatch; the credential `selectOption` calls are deleted. In `e2e/tests/10-full-stack.spec.ts` the `TTS voice` label becomes `Voice` and the model fields use the same rule.
  - `e2e/global-setup.ts` (paid Google suite) passes `E2E_GOOGLE_SECRET` as `GOOGLE_APPLICATION_CREDENTIALS` (written to a temp file) and `E2E_GOOGLE_LLM_SECRET` as `GOOGLE_API_KEY`; `10-full-stack.spec.ts` drops the credential-adding UI step and its masked-secret assertions, keeps "the secret is never echoed into the page" as an assertion on the providers page.
  - `tools/livecheck`: `provision` stops calling `storeCreds`/`credPlan`; `startServer` passes the profile's `requires` and `optional` variables (values from the harness environment) into the child through `childEnvAllow`-style filtering limited to exactly those names; `agentConfig` drops `credential_id`; `provisionFailover` becomes a failover agent whose primary points at a provider whose variable is deliberately wrong: the child gets the broken key for the primary provider and the fallback is a different provider (`xai` primary with a broken `XAI_API_KEY` cannot also be the fallback: S13 now uses two providers, documented in its `na` reason when the profile has only one).
- [ ] **Step 3: Run** `cd server && go test -race -tags "xaispeech fakeproviders" -run GraphE2E ./ -count=1`; `make e2e-fake`; `go test -race ./tools/livecheck/ -count=1`; all PASS. Live checks are in Task 12.
- [ ] **Step 4: Commit** `refactor: dev flow, e2e and livecheck use environment credentials`.

---

### Task 12: Docs, deploy, hardening tests, verification and review

**Files:**
- Modify/rewrite: `docs/security/credentials.md`, `docs/api/v1.md`, `docs/api/mcp.md`, `docs/quickstart.md`, `README.md`, `docs/operations/*.md`, `docs/privacy.md`, `docs/pipecat-parity-matrix.md` (provider rows), `deploy/docker-compose.yaml`, `deploy/govoice.service`, `deploy/smoke*.sh`
- Create: `docs/providers.md` (the catalog: file format, adding a provider/model/voice, the overlay directory, environment variable table), `server/secret_leak_test.go`

- [ ] **Step 1: Failing hardening tests.** `server/secret_leak_test.go` `TestNoSecretLeavesTheServer` (Review Focus 1): set `s.env` with sentinel values (`sk-SENTINEL-xai`, a service-account file containing `SENTINEL-private-key`, `MCP_TOKEN_CRM=SENTINEL-mcp`); create an agent, a graph agent, an MCP server with `auth_env`; run a fake-provider call (reuse the `-tags fakeproviders` rig by placing this test in `graph_e2e_test.go`'s build tag file group); then fetch every management route (`/providers`, `/agents`, `/agents/{id}`, `/mcp-servers`, `/sessions`, `/sessions/{id}/traces`, `/debug/timeline`), the metrics endpoint and the captured server log; assert none of the sentinels appears anywhere. `TestHostileOverlayIsSkipped` is Task 2's (Review Focus 4); here add an integration check that `NewServer` with a bad overlay directory starts.
- [ ] **Step 2: Run, expect PASS or fix** the leak it finds (any failure is a Critical bug: fix at the source, never by redacting in the test).
- [ ] **Step 3: Docs.** Rewrite `docs/security/credentials.md` to the env-only model (what is read, when, who can see it, rotation = redeploy, MCP prefix rule). `docs/api/v1.md`: remove the credential routes and `credential_id`, document `GET /providers`, `missing_env`, `auth_env`. `docs/quickstart.md`/`README.md`: replace "add a credential" with "set the variables", list the variable table, and add a release note: *"delete `.govoice-dev` (or any development database): the schema changed in place; nothing is migrated."* `deploy/*`: remove `GOVOICE_CREDENTIAL_KEYS`, add provider variables via `EnvironmentFile`/`env_file`. `docs/providers.md` as above.
- [ ] **Step 4: Live verification** (spends credit; ask before running if the keys are not already present): `GOVOICE_LIVE=1 make e2e-xai` and `go run ./tools/livecheck -profiles xai -modes off -scenarios S14 -max-calls 15 -yes`; both must pass with the environment-only setup.
- [ ] **Step 5: Gate:** `gofmt -w . && make verify && (cd ui && npx vitest run && npx tsc --noEmit && npm run build) && make test-graph-e2e && make e2e-fake` → all PASS.
- [ ] **Step 6: Independent adversarial review** of `git diff main...HEAD` per AGENTS.md: YAGNI/bloat (is every catalog feature consumed by the server and UI?), secret handling and logging, races (the resolver is stateless; `Server.env` is set only before serving), the seven Review Focus lines, deleted-code completeness (`grep -rn "credential" server ui/src --include='*.go' --include='*.ts*' -i` leaves only unrelated uses such as trace redaction keys and the word in comments about secrets). Fix findings with a failing test first; re-run Step 5.
- [ ] **Step 7: Commit** `docs: environment-only credentials, provider catalog guide, release note`.
