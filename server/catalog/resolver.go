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
}

// Resolver reads credentials from the environment on every call; it caches nothing.
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

// readLimited reads a regular file of at most max bytes. The type is checked
// before opening: opening a FIFO for reading blocks until a writer appears.
func readLimited(path string, max int64) ([]byte, error) {
	if st, err := os.Stat(path); err != nil {
		return nil, err
	} else if !st.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
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
// variables that are required but unset, plus every set variable whose value is
// unusable: an optional field that is set but bad (OPENAI_BASE_URL=$GROQ_URL)
// must not be dropped, or the key would go to the default endpoint.
func (r *Resolver) values(k *Kind) (vals map[string]string, missing []string) {
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
			// file_json is a service-account key: a user ADC file or any other JSON
			// would be handed to the adapter only to fail at call time.
			b, err := r.readFile(v, MaxFileBytes)
			var sa struct {
				Type string `json:"type"`
			}
			if err != nil || json.Unmarshal(b, &sa) != nil || sa.Type != "service_account" {
				missing = append(missing, f.Env)
				continue
			}
			compact, _ := json.Marshal(json.RawMessage(b))
			vals[id] = string(compact)
			continue
		}
		if strings.HasPrefix(v, "$") || strings.IndexFunc(v, func(c rune) bool { return c <= ' ' || c == 0x7f }) >= 0 {
			missing = append(missing, f.Env)
			continue
		}
		vals[id] = v
	}
	return vals, missing
}

// satisfies reports whether kind k can serve svc, and what is missing if not.
func (r *Resolver) satisfies(k *Kind, svc *Service) (map[string]string, []string, bool) {
	vals, missing := r.values(k)
	for _, need := range svc.Needs {
		f, has := k.Fields[need]
		if !has {
			return nil, nil, false
		}
		if _, got := vals[need]; !got {
			missing = append(missing, f.Env)
		}
	}
	return vals, uniq(missing), len(missing) == 0
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
	if svc.Keyless {
		return Status{Declared: true, Detected: true}
	}
	var firstMissing []string
	for i, kid := range svc.Credential {
		k, _ := p.Kind(kid)
		_, missing, ok := r.satisfies(k, svc)
		if ok {
			return Status{Declared: true, Detected: true}
		}
		if i == 0 {
			firstMissing = missing
		}
	}
	return Status{Declared: true, MissingEnv: firstMissing}
}

// Secret assembles a fresh buffer; the caller wipes it.
func (r *Resolver) Secret(provider, service string) ([]byte, error) {
	p, svc, ok := r.service(provider, service)
	if !ok {
		return nil, ErrUnknown
	}
	if svc.Keyless {
		return nil, nil
	}
	var firstMissing []string
	for i, kid := range svc.Credential {
		k, _ := p.Kind(kid)
		vals, missing, ok := r.satisfies(k, svc)
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

// Env lists every variable the provider's credential kinds read, sorted.
func (r *Resolver) Env(provider string) []string {
	p, ok := r.cat.Provider(provider)
	if !ok {
		return nil
	}
	var out []string
	for i := range p.Credentials {
		for _, f := range p.Credentials[i].Fields {
			out = append(out, f.Env)
		}
	}
	return uniq(out)
}

// Detected reports whether any service the provider declares is usable now.
func (r *Resolver) Detected(provider string) bool {
	p, ok := r.cat.Provider(provider)
	if !ok {
		return false
	}
	for name := range p.Services {
		if r.Status(provider, name).Detected {
			return true
		}
	}
	return false
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
