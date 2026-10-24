package secrets

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"

	"govoice/server/api"
	"govoice/server/storage"
)

// SecretView is a secret without its value. Writes go through PUT and the value is never read back by any endpoint.
type SecretView struct {
	Name       string     `json:"name"`
	Source     string     `json:"source"`   // environment, directory or store
	Writable   bool       `json:"writable"` // only the store can be changed through the API
	UpdatedAt  *time.Time `json:"updated_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// SecretList is the list response. StoreEnabled says whether PUT can work (the master key is set).
type SecretList struct {
	StoreEnabled bool         `json:"store_enabled"`
	Secrets      []SecretView `json:"secrets"`
	// RecentChanges is the audit trail of stored secrets, newest first (at most recentChanges): what happened to which
	// name, and the request that did it. Never a value.
	RecentChanges []Change `json:"recent_changes"`
}

// Change is one audited change to a stored secret.
type Change struct {
	At        time.Time `json:"at"`
	Action    string    `json:"action"` // create, replace or delete
	Name      string    `json:"name"`
	RequestID string    `json:"request_id"`
}

const recentChanges = 20

// Sources are the backends the handler lists and writes to.
type Sources struct {
	Env   Env
	Dir   Dir
	Store Store
	// Uses lists what besides MCP servers refers to a secret (HTTP tools); it may be nil.
	Uses    func(ctx context.Context, name string) ([]storage.SecretUse, error)
	Environ func() []string // defaults to os.Environ
	Logger  *slog.Logger
}

// Handler exposes /api/v1/secrets. It must only be mounted behind the management boundary, which authenticates first.
func Handler(src Sources) http.Handler {
	environ := src.Environ
	if environ == nil {
		environ = os.Environ
	}
	log := src.Logger
	if log == nil {
		log = slog.Default()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/secrets", func(w http.ResponseWriter, r *http.Request) {
		id := api.RequestID(w, r)
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			api.WriteError(w, 405, "method_not_allowed", "method is not supported", id)
			return
		}
		views := []SecretView{}
		for _, i := range src.Env.List(environ()) {
			views = append(views, SecretView{Name: i.Name, Source: i.Source})
		}
		for _, i := range src.Dir.List() {
			views = append(views, SecretView{Name: i.Name, Source: i.Source})
		}
		if src.Store.Enabled() {
			stored, err := src.Store.List(r.Context())
			if err != nil {
				api.WriteError(w, 500, "internal_error", "secrets unavailable", id)
				return
			}
			for _, i := range stored {
				v := SecretView{Name: i.Name, Source: i.Source, Writable: true, UpdatedAt: &i.UpdatedAt}
				if !i.LastUsedAt.IsZero() {
					v.LastUsedAt = &i.LastUsedAt
				}
				views = append(views, v)
			}
		}
		list := SecretList{StoreEnabled: src.Store.Enabled(), Secrets: views, RecentChanges: []Change{}}
		if list.StoreEnabled {
			changes, err := src.Store.Repo.Changes(r.Context(), recentChanges)
			if err != nil {
				api.WriteError(w, 500, "internal_error", "secrets unavailable", id)
				return
			}
			for _, c := range changes {
				list.RecentChanges = append(list.RecentChanges, Change{At: c.At, Action: c.Action, Name: c.Name, RequestID: c.RequestID})
			}
		}
		api.WriteJSON(w, 200, api.SuccessResponse[SecretList]{Data: list})
	})
	mux.HandleFunc("/api/v1/secrets/{name}", func(w http.ResponseWriter, r *http.Request) {
		id := api.RequestID(w, r)
		name := r.PathValue("name")
		switch r.Method {
		case http.MethodPut:
			put(w, r, src, log, id, name)
		case http.MethodDelete:
			del(w, r, src, log, id, name)
		default:
			w.Header().Set("Allow", "PUT, DELETE")
			api.WriteError(w, 405, "method_not_allowed", "method is not supported", id)
		}
	})
	return mux
}

func put(w http.ResponseWriter, r *http.Request, src Sources, log *slog.Logger, id, name string) {
	if !src.Store.Enabled() {
		api.WriteError(w, 503, "secrets_store_disabled", "the encrypted secret store is off: set SWITCHBOARD_SECRETS_KEY and restart", id)
		return
	}
	if !ValidStoredName(name) {
		api.WriteError(w, 400, "invalid_secret", "name must be 1-63 characters: lower-case letters, digits, '-' or '_', starting with a letter or digit", id)
		return
	}
	var body struct {
		Value string `json:"value"`
	}
	if err := api.DecodeJSON(r, &body); err != nil {
		api.WriteError(w, 400, "invalid_request", err.Error(), id)
		return
	}
	if !ValidValue(body.Value) {
		api.WriteError(w, 400, "invalid_secret", "value must be 1-4096 characters with no spaces or control characters", id)
		return
	}
	// The chain tries the directory before the store, so a stored value of the same name would never be used.
	if _, ok := src.Dir.Resolve(r.Context(), name); ok {
		api.WriteError(w, 409, "secret_managed_externally", "a file in the secrets directory already has this name", id)
		return
	}
	if err := src.Store.Put(WithAudit(r.Context(), id), name, body.Value); err != nil {
		log.Error("secret write failed", slog.String("secret", name), slog.String("request_id", id))
		api.WriteError(w, 500, "internal_error", "secret could not be saved", id)
		return
	}
	log.Info("secret written", slog.String("secret", name), slog.String("request_id", id))
	api.WriteJSON(w, 200, api.SuccessResponse[SecretView]{Data: SecretView{Name: name, Source: sourceStore, Writable: true}})
}

func del(w http.ResponseWriter, r *http.Request, src Sources, log *slog.Logger, id, name string) {
	if !src.Store.Enabled() || !ValidStoredName(name) {
		api.WriteError(w, 404, "not_found", "secret not found", id)
		return
	}
	uses, err := src.Store.Repo.Uses(r.Context(), name)
	if err == nil && src.Uses != nil {
		var more []storage.SecretUse
		if more, err = src.Uses(r.Context(), name); err == nil {
			uses = append(uses, more...)
		}
	}
	if err != nil {
		api.WriteError(w, 500, "internal_error", "secrets unavailable", id)
		return
	}
	if len(uses) > 0 {
		api.WriteJSON(w, http.StatusConflict, api.ErrorResponse{Error: api.Error{Code: "secret_in_use", Message: "secret is used by tools or servers", RequestID: id, Details: uses}})
		return
	}
	switch err := src.Store.Delete(WithAudit(r.Context(), id), name); {
	case errors.Is(err, sql.ErrNoRows):
		api.WriteError(w, 404, "not_found", "secret not found", id)
	case err != nil:
		api.WriteError(w, 500, "internal_error", "secret could not be deleted", id)
	default:
		log.Info("secret deleted", slog.String("secret", name), slog.String("request_id", id))
		w.WriteHeader(http.StatusNoContent)
	}
}
