// Package httptool is the HTTP endpoint tool: an agent step can call one configured HTTP endpoint without an MCP
// server. A tool's definition is immutable and identified by its hash, so an agent version pins exactly what was
// reviewed; editing a tool creates a new definition. Arguments and results are never stored, as with MCP tools.
package httptool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"govoice/server/mcp"
	"govoice/server/secrets"
	"govoice/server/storage"
)

const (
	// RefPrefix starts the pseudo server id an HTTP tool selection carries ("http:<tool id>"), so a tool pin
	// shares the table and step references of MCP tool pins without colliding with a server id.
	RefPrefix = "http:"

	DefaultTimeout     = 10 * time.Second
	MaxTimeout         = 30 * time.Second
	DefaultMaxResponse = 64 << 10
	MaxMaxResponse     = 256 << 10
	MaxSchemaBytes     = 16 << 10
	MaxHeaders         = 16
	maxHeaderValue     = 512
	maxPlaceholders    = 8
	maxPick            = 10
	maxDescription     = 1024
	maxURL             = 2048
)

var (
	ErrInvalid = errors.New("invalid HTTP tool")

	nameRe        = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
	placeholderRe = regexp.MustCompile(`\{([a-z][a-z0-9_]{0,31})\}`)
	headerNameRe  = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]{1,64}$")
	methods       = []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}
	// Headers the transport owns, or that would let a definition smuggle a credential into the stored, readable part.
	deniedHeaders = []string{"Host", "Content-Length", "Transfer-Encoding", "Connection", "Upgrade", "Te", "Trailer", "Keep-Alive", "Cookie", "Set-Cookie", "Proxy-Authorization", "Proxy-Connection"}
	bodyMethods   = []string{http.MethodPost, http.MethodPut, http.MethodPatch}
)

// Auth names the one secret header of a tool. Secret is a name for the secrets resolver, never a value.
type Auth struct {
	Header string `json:"header,omitempty"` // default Authorization; "Authorization" sends "Bearer <value>", any other header the raw value
	Secret string `json:"secret"`
}

// Definition is everything a call needs. It carries no secret value and no argument or result.
type Definition struct {
	Name             string            `json:"name"`
	Description      string            `json:"description"`
	Method           string            `json:"method"`
	URL              string            `json:"url"` // https://host[:port]/path with optional {placeholders} in the path
	Headers          map[string]string `json:"headers,omitempty"`
	Auth             *Auth             `json:"auth,omitempty"`
	InputSchema      json.RawMessage   `json:"input_schema"`
	TimeoutMS        int               `json:"timeout_ms,omitempty"`
	MaxResponseBytes int               `json:"max_response_bytes,omitempty"`
	Pick             []string          `json:"response_pick,omitempty"` // JSON pointers; only these parts of a JSON body reach the model
}

// Options are the deployment switches that change what is acceptable.
type Options struct {
	// AllowLocalHTTP also allows http to loopback (development and tests; production refuses the config switch).
	AllowLocalHTTP bool
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Reason is the part of a validation error that is safe to show a caller.
func Reason(err error) string {
	return strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": ")
}

// Ref is the pseudo server id of a tool selection.
func Ref(toolID string) string { return RefPrefix + toolID }

// IDFromRef returns the tool id of an HTTP tool selection's server id.
func IDFromRef(serverID string) (string, bool) {
	id, ok := strings.CutPrefix(serverID, RefPrefix)
	return id, ok && id != ""
}

// Normalize checks a definition and returns its canonical form with defaults filled in. It does not touch the
// network: the destination is checked by the service, which may resolve DNS.
func (d Definition) Normalize(ctx context.Context, opts Options) (Definition, error) {
	d.Name = strings.TrimSpace(d.Name)
	if !nameRe.MatchString(d.Name) {
		return d, invalid("name must be 1-40 characters: lower-case letters, digits and _, starting with a letter")
	}
	if strings.TrimSpace(d.Description) == "" || len(d.Description) > maxDescription {
		return d, invalid("description is required (it tells the model when to use the tool) and at most %d characters", maxDescription)
	}
	d.Method = strings.ToUpper(strings.TrimSpace(d.Method))
	if !slices.Contains(methods, d.Method) {
		return d, invalid("method must be one of %s", strings.Join(methods, ", "))
	}
	if err := d.checkURL(opts); err != nil {
		return d, err
	}
	if err := d.checkHeaders(); err != nil {
		return d, err
	}
	if d.Auth != nil {
		if d.Auth.Header == "" {
			d.Auth.Header = "Authorization"
		}
		d.Auth.Header = textproto.CanonicalMIMEHeaderKey(d.Auth.Header)
		if !headerNameRe.MatchString(d.Auth.Header) || slices.Contains(deniedHeaders, d.Auth.Header) {
			return d, invalid("auth header is not allowed")
		}
		if !secrets.ValidName(d.Auth.Secret) || strings.HasPrefix(d.Auth.Secret, "MCP_TOKEN_") {
			return d, invalid("auth secret must be an HTTP_TOKEN_ variable name or a lower-case secret name such as crm-api")
		}
		if _, clash := d.Headers[d.Auth.Header]; clash {
			return d, invalid("header %s is set both as a static header and as the auth header", d.Auth.Header)
		}
	}
	schema, err := d.checkSchema(ctx)
	if err != nil {
		return d, err
	}
	d.InputSchema = schema
	switch {
	case d.TimeoutMS == 0:
		d.TimeoutMS = int(DefaultTimeout / time.Millisecond)
	case d.TimeoutMS < 100 || d.TimeoutMS > int(MaxTimeout/time.Millisecond):
		return d, invalid("timeout_ms must be between 100 and %d", MaxTimeout/time.Millisecond)
	}
	switch {
	case d.MaxResponseBytes == 0:
		d.MaxResponseBytes = DefaultMaxResponse
	case d.MaxResponseBytes < 256 || d.MaxResponseBytes > MaxMaxResponse:
		return d, invalid("max_response_bytes must be between 256 and %d", MaxMaxResponse)
	}
	if len(d.Pick) > maxPick {
		return d, invalid("response_pick lists at most %d pointers", maxPick)
	}
	for _, p := range d.Pick {
		if !strings.HasPrefix(p, "/") || len(p) > 256 || strings.ContainsAny(p, "\x00\r\n") {
			return d, invalid("response_pick entries are JSON pointers such as /data/status")
		}
	}
	return d, nil
}

// Hash identifies a normalized definition: the same definition always has the same hash, any edit changes it.
func (d Definition) Hash() (string, []byte, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return "", nil, err
	}
	return storage.ConfigHash(raw)
}

// Placeholders lists the path placeholders in order of appearance.
func (d Definition) Placeholders() []string {
	var out []string
	for _, m := range placeholderRe.FindAllStringSubmatch(d.URL, -1) {
		out = append(out, m[1])
	}
	return out
}

// Host is the scheme, host and port of the URL, which never depend on arguments.
func (d Definition) Host() (*url.URL, error) {
	u, err := url.Parse(placeholderRe.ReplaceAllString(d.URL, "x"))
	if err != nil {
		return nil, invalid("url is not valid")
	}
	return u, nil
}

func (d Definition) checkURL(opts Options) error {
	if d.URL == "" || len(d.URL) > maxURL {
		return invalid("url is required and at most %d characters", maxURL)
	}
	if strings.ContainsAny(d.URL, "?#") {
		return invalid("url must not contain a query string or fragment; query parameters come from the arguments")
	}
	rest := placeholderRe.ReplaceAllString(d.URL, "x")
	if strings.ContainsAny(rest, "{}") {
		return invalid("placeholders are written {name} with a lower-case name")
	}
	if len(placeholderRe.FindAllString(d.URL, -1)) > maxPlaceholders {
		return invalid("at most %d placeholders", maxPlaceholders)
	}
	u, err := url.Parse(rest)
	if err != nil || u.Host == "" || u.User != nil || u.Hostname() == "" {
		return invalid("url must be an absolute https URL without embedded credentials")
	}
	if _, after, _ := strings.Cut(d.URL, "://"); strings.ContainsAny(strings.SplitN(after, "/", 2)[0], "{}") {
		return invalid("placeholders are allowed in the path only")
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && opts.AllowLocalHTTP:
	default:
		return invalid("url must use https")
	}
	seen := map[string]bool{}
	for _, name := range d.Placeholders() {
		if seen[name] {
			return invalid("placeholder {%s} appears twice", name)
		}
		seen[name] = true
	}
	return nil
}

func (d *Definition) checkHeaders() error {
	if len(d.Headers) > MaxHeaders {
		return invalid("at most %d static headers", MaxHeaders)
	}
	canonical := make(map[string]string, len(d.Headers))
	for name, value := range d.Headers {
		key := textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(name))
		if !headerNameRe.MatchString(key) {
			return invalid("header name %q is not valid", name)
		}
		if slices.Contains(deniedHeaders, key) || key == "Authorization" {
			return invalid("header %s cannot be set here; credentials go in the auth setting", key)
		}
		if len(value) > maxHeaderValue || strings.ContainsFunc(value, func(r rune) bool { return r < ' ' && r != '\t' || r == 0x7f }) {
			return invalid("header %s has a value that is too long or has control characters", key)
		}
		if _, dup := canonical[key]; dup {
			return invalid("header %s appears twice", key)
		}
		canonical[key] = value
	}
	if len(canonical) == 0 {
		canonical = nil
	}
	d.Headers = canonical
	return nil
}

// checkSchema validates the input schema with the hardened MCP checks and requires every placeholder to be a
// required string or integer property. It returns the canonical schema.
func (d Definition) checkSchema(ctx context.Context) (json.RawMessage, error) {
	if len(d.InputSchema) == 0 || len(d.InputSchema) > MaxSchemaBytes {
		return nil, invalid("input_schema is required and at most %d bytes", MaxSchemaBytes)
	}
	if err := mcp.CheckInputSchema(ctx, d.InputSchema); err != nil {
		return nil, invalid("input_schema must be a valid JSON Schema of type object")
	}
	canonical, err := storage.CanonicalJSON(d.InputSchema)
	if err != nil {
		return nil, invalid("input_schema is not valid JSON")
	}
	var s struct {
		Properties map[string]struct {
			Type any `json:"type"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(canonical, &s); err != nil {
		return nil, invalid("input_schema is not valid JSON")
	}
	for _, name := range d.Placeholders() {
		prop, ok := s.Properties[name]
		typ, _ := prop.Type.(string)
		if !ok || (typ != "string" && typ != "integer") || !slices.Contains(s.Required, name) {
			return nil, invalid("placeholder {%s} must be a required string or integer property of input_schema", name)
		}
	}
	return canonical, nil
}
