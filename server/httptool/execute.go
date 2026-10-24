package httptool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"govoice/server/mcp"
)

// ErrSecretUnavailable means the tool's secret name resolves to nothing (maps to the "unavailable" category).
var ErrSecretUnavailable = errors.New("HTTP tool secret is unavailable")

// ErrUnavailable is any failure to reach the endpoint; the cause is never shown to the model.
var ErrUnavailable = errors.New("HTTP tool endpoint is unavailable")

// Resolve returns a secret's value; ok is false when the name does not resolve.
type Resolve func(ctx context.Context, name string) (value string, ok bool)

// Executor makes the call a definition describes. It holds no per-tool state.
type Executor struct {
	Client *http.Client
	Secret Resolve
}

// Execute validates the arguments, builds the request, calls the endpoint within the definition's limits and returns
// {"status": n, "body": ...}. Errors carry an mcp category (mcp.ErrorCategory): invalid arguments, timeout,
// cancelled, result too large, a tool error (the service answered with an error status or something unreadable) or
// unavailable.
func (e *Executor) Execute(ctx context.Context, def Definition, args []byte) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := mcp.ValidateArguments(ctx, def.InputSchema, args); err != nil {
		return nil, mcp.ErrInvalidArguments
	}
	values, err := decodeObject(args)
	if err != nil {
		return nil, mcp.ErrInvalidArguments
	}
	target, err := def.expand(values)
	if err != nil {
		return nil, mcp.ErrInvalidArguments
	}
	var token string
	if def.Auth != nil {
		if strings.HasPrefix(def.Auth.Secret, "MCP_TOKEN_") { // refused at save time; a row written another way still never sends one
			return nil, ErrSecretUnavailable
		}
		var ok bool
		if token, ok = e.Secret(ctx, def.Auth.Secret); !ok {
			return nil, ErrSecretUnavailable
		}
	}
	var body io.Reader
	if slices.Contains(bodyMethods, def.Method) {
		raw, err := json.Marshal(values)
		if err != nil {
			return nil, mcp.ErrInvalidArguments
		}
		body = bytes.NewReader(raw)
	} else if err := target.addQuery(values); err != nil {
		return nil, mcp.ErrInvalidArguments
	}
	ctx, cancel := context.WithTimeout(ctx, def.timeout())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, def.Method, target.String(), body)
	if err != nil {
		return nil, mcp.ErrInvalidArguments
	}
	for k, v := range def.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json, text/plain;q=0.8")
	req.Header.Set("User-Agent", "switchboard-http-tool")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if def.Auth != nil {
		if def.Auth.Header == "Authorization" {
			req.Header.Set("Authorization", "Bearer "+token)
		} else {
			req.Header.Set(def.Auth.Header, token)
		}
	}
	resp, err := e.Client.Do(req)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return nil, context.DeadlineExceeded
		case errors.Is(err, context.Canceled):
			return nil, context.Canceled
		}
		return nil, ErrUnavailable // a transport error text can carry the URL and the resolved address
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &mcp.ToolError{Message: fmt.Sprintf("the service answered HTTP %d", resp.StatusCode)}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, int64(def.MaxResponseBytes)+1))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, ErrUnavailable
	}
	if len(raw) > def.MaxResponseBytes {
		return nil, mcp.ErrResultTooLarge
	}
	parsed, err := parseBody(resp.Header.Get("Content-Type"), raw)
	if err != nil {
		return nil, err
	}
	if len(def.Pick) > 0 {
		parsed = pick(parsed, def.Pick)
	}
	out, err := json.Marshal(struct {
		Status int `json:"status"`
		Body   any `json:"body"`
	}{resp.StatusCode, parsed})
	if err != nil {
		return nil, ErrUnavailable
	}
	// The limit is on the endpoint's bytes; JSON escaping of <, > and & can grow them (up to six times).
	clean, err := mcp.SanitizeResult(out, token, def.MaxResponseBytes*6+1024)
	if err != nil {
		return nil, mcp.ErrResultTooLarge
	}
	return clean, nil
}

func (d Definition) timeout() time.Duration { return time.Duration(d.TimeoutMS) * time.Millisecond }

func decodeObject(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v map[string]any
	if err := dec.Decode(&v); err != nil || v == nil {
		return nil, errors.New("arguments must be a JSON object")
	}
	return v, nil
}

// target is the URL being built: path placeholders are already filled, the query is added for body-less methods.
type target struct {
	u *url.URL
}

func (t *target) String() string { return t.u.String() }

// expand fills the path placeholders from values (removing them from it) and returns the request target. A value is
// a string or an integer and may not change the shape of the path: no separators, no dot segments, no controls.
func (d Definition) expand(values map[string]any) (*target, error) {
	path := d.URL
	for _, name := range d.Placeholders() {
		seg, err := segment(values[name])
		if err != nil {
			return nil, err
		}
		delete(values, name)
		path = strings.Replace(path, "{"+name+"}", url.PathEscape(seg), 1)
	}
	u, err := url.Parse(path)
	if err != nil {
		return nil, err
	}
	return &target{u}, nil
}

func segment(v any) (string, error) {
	var s string
	switch x := v.(type) {
	case string:
		s = x
	case json.Number:
		if _, err := x.Int64(); err != nil {
			return "", errors.New("placeholder must be an integer")
		}
		s = x.String()
	default:
		return "", errors.New("placeholder value missing")
	}
	if s == "" || s == "." || s == ".." || strings.ContainsAny(s, "/\\") || strings.ContainsFunc(s, func(r rune) bool { return r < ' ' || r == 0x7f }) {
		return "", errors.New("placeholder value is not a safe path segment")
	}
	return s, nil
}

// addQuery sends the remaining arguments as query parameters: scalars, or lists of scalars as repeated parameters.
func (t *target) addQuery(values map[string]any) error {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	q := url.Values{}
	for _, k := range keys {
		switch x := values[k].(type) {
		case []any:
			for _, item := range x {
				s, ok := scalar(item)
				if !ok {
					return errors.New("query values must be scalars")
				}
				q.Add(k, s)
			}
		default:
			s, ok := scalar(x)
			if !ok {
				return errors.New("query values must be scalars")
			}
			q.Set(k, s)
		}
	}
	t.u.RawQuery = q.Encode()
	return nil
}

func scalar(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case json.Number:
		return x.String(), true
	case bool:
		return strconv.FormatBool(x), true
	}
	return "", false
}

// parseBody turns a response into a value the model can read: JSON as JSON, plain text as a string, nothing as null.
func parseBody(contentType string, raw []byte) (any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	media, _, err := mime.ParseMediaType(contentType)
	switch {
	case err == nil && (media == "application/json" || strings.HasSuffix(media, "+json")):
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil || dec.More() {
			return nil, &mcp.ToolError{Message: "the service sent invalid JSON"}
		}
		return v, nil
	case err == nil && media == "text/plain":
		if !utf8.Valid(raw) {
			return nil, &mcp.ToolError{Message: "the service sent text that is not valid UTF-8"}
		}
		return string(raw), nil
	}
	return nil, &mcp.ToolError{Message: "the service answered with a content type tools cannot read"}
}

// pick keeps only the parts of a JSON body the JSON pointers name, keyed by pointer. Pointers that match nothing,
// or a body that is not JSON, leave nothing for them.
func pick(body any, pointers []string) any {
	switch body.(type) {
	case map[string]any, []any:
	default:
		return body
	}
	out := make(map[string]any, len(pointers))
	for _, p := range pointers {
		if v, ok := lookup(body, p); ok {
			out[p] = v
		}
	}
	return out
}

func lookup(v any, pointer string) (any, bool) {
	for _, part := range strings.Split(pointer[1:], "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		switch x := v.(type) {
		case map[string]any:
			next, ok := x[part]
			if !ok {
				return nil, false
			}
			v = next
		case []any:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(x) {
				return nil, false
			}
			v = x[i]
		default:
			return nil, false
		}
	}
	return v, true
}
