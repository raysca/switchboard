package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"govoice/server/agent"
	"govoice/server/eval"
	"govoice/server/storage"
)

// Bounds of the eval endpoints. The body limit covers a whole batch of cases; one case is bounded on its own.
const (
	evalMaxYAMLBody   = 1 << 20 // bytes of a case request
	evalMaxCaseName   = 200     // runes
	evalMaxCaseBytes  = 16 << 10
	evalMaxReason     = 1000 // runes of a score reason on the way out
	evalMaxJudgeField = 128
	evalYAMLSource    = "cases.yaml" // what a parse error calls the request's YAML
)

// EvalAgents is what the eval endpoints need of the agent service.
type EvalAgents interface {
	Get(ctx context.Context, id string) (agent.Agent, error)
	VersionByNumber(ctx context.Context, id string, number int) (agent.Version, error)
	ResolveDraft(ctx context.Context, id string, config json.RawMessage, selection *[]agent.ToolSelection) (agent.Snapshot, agent.Readiness, error)
}

// EvalStore is the storage the eval endpoints use (a *storage.Store).
type EvalStore interface {
	Eval() storage.EvalRepository
	Agents() storage.AgentRepository
}

// EvalPolicy is what the server decides for the eval endpoints.
type EvalPolicy struct {
	// LiveRequired reports that running this (rewritten) config reaches a paid model and the serving process has no
	// GOVOICE_LIVE=1: the gate of a paid run.
	LiveRequired func(config json.RawMessage) bool
	// JudgeLiveRequired is the same for a judge on that provider.
	JudgeLiveRequired func(provider string) bool
	// PersonaLiveRequired is the same for a persona caller, which asks the offline provider's model: paid when that
	// endpoint is remote.
	PersonaLiveRequired func() bool
	// JudgeProvider returns an error unless the provider can complete text (it does not look at keys).
	JudgeProvider func(provider string) error
}

// NewEvalHandler serves /api/v1/agents/{id}/eval/... behind management authentication.
func NewEvalHandler(agents EvalAgents, store EvalStore, policy EvalPolicy) http.Handler {
	return evalHandler{agents: agents, store: store, policy: policy}
}

type evalHandler struct {
	agents EvalAgents
	store  EvalStore
	policy EvalPolicy
}

// EvalCaseDTO is one saved case. YAML is the case in the file format (without an agent name), for the editor.
type EvalCaseDTO struct {
	ID             string      `json:"id"`
	Name           string      `json:"name"`
	Caller         eval.Caller `json:"caller"`
	Expect         eval.Expect `json:"expect"`
	ExpectedAnswer string      `json:"expected_answer,omitempty"`
	YAML           string      `json:"yaml"`
	CreatedAt      time.Time   `json:"created_at"`
	UpdatedAt      time.Time   `json:"updated_at"`
}

// EvalJudgeDTO names a judge: a configured LLM provider and optionally one of its models.
type EvalJudgeDTO struct {
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
}

// EvalSuiteDTO is the per-agent suite setting.
type EvalSuiteDTO struct {
	Runs          int           `json:"runs"`
	MinPass       int           `json:"min_pass"`
	PassThreshold float64       `json:"pass_threshold"`
	Judge         *EvalJudgeDTO `json:"judge"`
}

// EvalScorerDTO is a built-in scorer merged with the agent's setting for it.
type EvalScorerDTO struct {
	Name             string        `json:"name"`
	Judged           bool          `json:"judged"`
	DefaultThreshold float64       `json:"default_threshold"`
	Threshold        *float64      `json:"threshold,omitempty"`
	Judge            *EvalJudgeDTO `json:"judge,omitempty"`
}

// EvalRunDTO is one run. Version is absent for a draft run.
type EvalRunDTO struct {
	ID             string     `json:"id"`
	AgentID        string     `json:"agent_id"`
	Version        *int       `json:"version,omitempty"`
	AgentVersionID string     `json:"agent_version_id,omitempty"`
	Draft          bool       `json:"draft"`
	ConfigHash     string     `json:"config_hash"`
	Scope          string     `json:"scope"`
	CaseID         string     `json:"case_id,omitempty"`
	Providers      string     `json:"providers"`
	Runs           int        `json:"runs"`
	MinPass        int        `json:"min_pass"`
	Status         string     `json:"status"`
	TriggeredBy    string     `json:"triggered_by"`
	PassedCases    *int       `json:"passed_cases,omitempty"`
	TotalCases     *int       `json:"total_cases,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	EndedAt        *time.Time `json:"ended_at,omitempty"`
}

// EvalScoreDTO is one scorer's verdict on one case-run.
type EvalScoreDTO struct {
	Scorer     string  `json:"scorer"`
	Score      float64 `json:"score"`
	Outcome    string  `json:"outcome"`
	Reason     string  `json:"reason"`
	JudgeModel string  `json:"judge_model,omitempty"`
}

// EvalCaseRunDTO is one case at one repeat. ErrorCategory names why a call could not be made or read back (a dead
// job): a category, never content. Passed is true only for a finished case-run no scorer failed or errored.
type EvalCaseRunDTO struct {
	CaseID        string         `json:"case_id"`
	CaseName      string         `json:"case_name"`
	Repeat        int            `json:"repeat"`
	Status        string         `json:"status"`
	ErrorCategory string         `json:"error_category,omitempty"`
	SessionID     string         `json:"session_id,omitempty"`
	Passed        bool           `json:"passed"`
	Scores        []EvalScoreDTO `json:"scores"`
}

// EvalRunDetailDTO is a run with its case-runs.
type EvalRunDetailDTO struct {
	Run      EvalRunDTO       `json:"run"`
	CaseRuns []EvalCaseRunDTO `json:"case_runs"`
}

type evalYAMLRequest struct {
	YAML string `json:"yaml"`
}

func (h evalHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rid := RequestID(w, r)
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/agents/"), "/")
	if !strings.HasPrefix(r.URL.Path, "/api/v1/agents/") || len(parts) < 2 || parts[0] == "" || parts[1] != "eval" {
		WriteError(w, http.StatusNotFound, "not_found", "not found", rid)
		return
	}
	id, sub := parts[0], parts[2:]
	if _, err := h.store.Agents().Current(r.Context(), id); err != nil {
		h.fail(w, err, rid)
		return
	}
	switch {
	case len(sub) == 1 && sub[0] == "cases":
		h.cases(w, r, id, rid)
	case len(sub) == 2 && sub[0] == "cases" && sub[1] == "export":
		h.export(w, r, id, rid)
	case len(sub) == 2 && sub[0] == "cases":
		h.oneCase(w, r, id, sub[1], rid)
	case len(sub) == 1 && sub[0] == "suite":
		h.suite(w, r, id, rid)
	case len(sub) == 1 && sub[0] == "scorers":
		h.scorers(w, r, id, rid)
	case len(sub) == 2 && sub[0] == "scorers":
		h.putScorer(w, r, id, sub[1], rid)
	case len(sub) == 1 && sub[0] == "runs":
		h.runs(w, r, id, rid)
	case len(sub) == 2 && sub[0] == "runs":
		h.runDetail(w, r, id, sub[1], rid)
	default:
		WriteError(w, http.StatusNotFound, "not_found", "not found", rid)
	}
}

// fail maps a store error. Anything not named here is a 500 with a fixed message.
func (h evalHandler) fail(w http.ResponseWriter, err error, rid string) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		WriteError(w, http.StatusNotFound, "not_found", "not found", rid)
	case errors.Is(err, storage.ErrDuplicateCaseName):
		WriteError(w, http.StatusConflict, "duplicate_case_name", err.Error(), rid)
	case errors.Is(err, storage.ErrTooManyCases):
		WriteError(w, http.StatusConflict, "too_many_cases", fmt.Sprintf("an agent keeps at most %d cases", storage.MaxEvalCases), rid)
	case errors.Is(err, storage.ErrQueueFull):
		WriteError(w, http.StatusTooManyRequests, "eval_queue_full", fmt.Sprintf("an agent has at most %d pending or running eval jobs; wait for the queue to drain", storage.MaxPendingJobsPerAgent), rid)
	case errors.Is(err, storage.ErrNoJobs):
		WriteError(w, http.StatusConflict, "no_cases", "the agent has no cases to run", rid)
	default:
		WriteError(w, http.StatusInternalServerError, "internal_error", "eval request failed", rid)
	}
}

// methods reports whether the request uses one of the allowed methods; otherwise it writes the 405.
func methods(w http.ResponseWriter, r *http.Request, rid string, allowed ...string) bool {
	for _, m := range allowed {
		if r.Method == m {
			return true
		}
	}
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not supported", rid)
	return false
}

// onlyQuery refuses a query key outside keys: a stale name must not be ignored.
func onlyQuery(w http.ResponseWriter, r *http.Request, rid string, keys ...string) bool {
	for k := range r.URL.Query() {
		known := false
		for _, a := range keys {
			known = known || k == a
		}
		if !known {
			WriteError(w, http.StatusBadRequest, "invalid_request", "unknown query parameter "+strconv.Quote(k), rid)
			return false
		}
	}
	return true
}

// body decodes a JSON body of at most max bytes and writes the error (413 over the limit).
func body(w http.ResponseWriter, r *http.Request, rid string, dst any, max int) bool {
	err := DecodeJSONLimit(r, dst, max)
	switch {
	case err == nil:
		return true
	case errors.Is(err, ErrBodyTooLarge):
		WriteError(w, http.StatusRequestEntityTooLarge, "payload_too_large", err.Error(), rid)
	default:
		WriteError(w, http.StatusBadRequest, "invalid_request", err.Error(), rid)
	}
	return false
}

func clip(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max-3]) + "..."
}

// ---- cases

func caseFromRow(c storage.EvalCase) (eval.Case, error) {
	out := eval.Case{Name: c.Name, ExpectedAnswer: c.ExpectedAnswer}
	if err := json.Unmarshal(c.CallerJSON, &out.Caller); err != nil {
		return out, fmt.Errorf("stored case caller: %w", err)
	}
	if err := json.Unmarshal(c.ExpectJSON, &out.Expect); err != nil {
		return out, fmt.Errorf("stored case expectations: %w", err)
	}
	return out, nil
}

func caseToRow(c eval.Case) (storage.EvalCase, error) {
	caller, err := json.Marshal(c.Caller)
	if err != nil {
		return storage.EvalCase{}, fmt.Errorf("encode case caller: %w", err)
	}
	expect, err := json.Marshal(c.Expect)
	if err != nil {
		return storage.EvalCase{}, fmt.Errorf("encode case expectations: %w", err)
	}
	return storage.EvalCase{Name: c.Name, CallerJSON: caller, ExpectJSON: expect, ExpectedAnswer: c.ExpectedAnswer}, nil
}

// caseHash identifies a case's content: the SHA-256 of its canonical JSON without the agent and file.
func caseHash(c eval.Case) (string, error) {
	c.Agent, c.File = "", ""
	raw, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("encode case: %w", err)
	}
	canonical, err := storage.CanonicalJSON(raw)
	if err != nil {
		return "", fmt.Errorf("canonicalize case: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func caseDTO(row storage.EvalCase) (EvalCaseDTO, error) {
	c, err := caseFromRow(row)
	if err != nil {
		return EvalCaseDTO{}, err
	}
	y, err := yaml.Marshal(c)
	if err != nil {
		return EvalCaseDTO{}, err
	}
	return EvalCaseDTO{ID: row.ID, Name: c.Name, Caller: c.Caller, Expect: c.Expect, ExpectedAnswer: c.ExpectedAnswer, YAML: string(y), CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}, nil
}

func caseDTOs(rows []storage.EvalCase) ([]EvalCaseDTO, error) {
	out := make([]EvalCaseDTO, 0, len(rows))
	for _, row := range rows {
		d, err := caseDTO(row)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// parseCases reads the request's YAML (the CLI's loader, so the same errors naming cases.yaml#N) and bounds each
// case. A stored case belongs to the agent whose endpoint it came through: a name in its agent key is dropped.
func parseCases(y string) ([]eval.Case, error) {
	cs, err := eval.ParseCases([]byte(y), evalYAMLSource)
	if err != nil {
		return nil, err
	}
	for i := range cs {
		c := &cs[i]
		c.Agent, c.File = "", ""
		if n := utf8.RuneCountInString(c.Name); n > evalMaxCaseName {
			return nil, fmt.Errorf("%s#%d: name: %d chars, at most %d", evalYAMLSource, i+1, n, evalMaxCaseName)
		}
		raw, err := json.Marshal(c)
		if err != nil {
			return nil, fmt.Errorf("%s#%d: encode case: %w", evalYAMLSource, i+1, err)
		}
		if len(raw) > evalMaxCaseBytes {
			return nil, fmt.Errorf("%s#%d: the case is %d bytes, at most %d", evalYAMLSource, i+1, len(raw), evalMaxCaseBytes)
		}
	}
	return cs, nil
}

// readCases decodes a {"yaml"} request and parses it; it writes the error and returns false when the request is bad.
func (h evalHandler) readCases(w http.ResponseWriter, r *http.Request, rid string) ([]eval.Case, bool) {
	var req evalYAMLRequest
	if !body(w, r, rid, &req, evalMaxYAMLBody) {
		return nil, false
	}
	cs, err := parseCases(req.YAML)
	if err != nil {
		WriteError(w, http.StatusBadRequest, "invalid_case", clip(err.Error(), 500), rid)
		return nil, false
	}
	return cs, true
}

func (h evalHandler) cases(w http.ResponseWriter, r *http.Request, agentID, rid string) {
	if !methods(w, r, rid, http.MethodGet, http.MethodPost) || !onlyQuery(w, r, rid) {
		return
	}
	if r.Method == http.MethodGet {
		rows, err := h.store.Eval().ListCases(r.Context(), agentID)
		h.writeCases(w, http.StatusOK, rows, err, rid)
		return
	}
	cs, ok := h.readCases(w, r, rid)
	if !ok {
		return
	}
	if len(cs) > storage.MaxEvalCases {
		h.fail(w, storage.ErrTooManyCases, rid)
		return
	}
	rows := make([]storage.EvalCase, 0, len(cs))
	for _, c := range cs {
		row, err := caseToRow(c)
		if err != nil {
			h.fail(w, err, rid)
			return
		}
		rows = append(rows, row)
	}
	saved, err := h.store.Eval().CreateCases(r.Context(), agentID, rows)
	h.writeCases(w, http.StatusCreated, saved, err, rid)
}

func (h evalHandler) writeCases(w http.ResponseWriter, status int, rows []storage.EvalCase, err error, rid string) {
	var out []EvalCaseDTO
	if err == nil {
		out, err = caseDTOs(rows)
	}
	if err != nil {
		h.fail(w, err, rid)
		return
	}
	WriteJSON(w, status, SuccessResponse[[]EvalCaseDTO]{Data: out})
}

// export serves every case as one multi-document YAML file the CLI loads, each case naming the agent.
func (h evalHandler) export(w http.ResponseWriter, r *http.Request, agentID, rid string) {
	if !methods(w, r, rid, http.MethodGet) || !onlyQuery(w, r, rid) {
		return
	}
	ag, err := h.store.Agents().Current(r.Context(), agentID)
	var rows []storage.EvalCase
	if err == nil {
		rows, err = h.store.Eval().ListCases(r.Context(), agentID)
	}
	if err != nil {
		h.fail(w, err, rid)
		return
	}
	var b strings.Builder
	for i, row := range rows {
		c, err := caseFromRow(row)
		if err == nil {
			c.Agent = ag.Name
			var y []byte
			if y, err = yaml.Marshal(c); err == nil {
				if i > 0 {
					b.WriteString("---\n")
				}
				b.Write(y)
			}
		}
		if err != nil {
			h.fail(w, err, rid)
			return
		}
	}
	WriteJSON(w, http.StatusOK, SuccessResponse[evalYAMLRequest]{Data: evalYAMLRequest{YAML: b.String()}})
}

func (h evalHandler) oneCase(w http.ResponseWriter, r *http.Request, agentID, caseID, rid string) {
	if !methods(w, r, rid, http.MethodGet, http.MethodPut, http.MethodDelete) || !onlyQuery(w, r, rid) {
		return
	}
	ctx, repo := r.Context(), h.store.Eval()
	switch r.Method {
	case http.MethodDelete:
		if err := repo.DeleteCase(ctx, agentID, caseID); err != nil {
			h.fail(w, err, rid)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodPut:
		cs, ok := h.readCases(w, r, rid)
		if !ok {
			return
		}
		if len(cs) != 1 {
			WriteError(w, http.StatusBadRequest, "invalid_case", fmt.Sprintf("a case is replaced by exactly one document, got %d", len(cs)), rid)
			return
		}
		row, err := caseToRow(cs[0])
		if err == nil {
			row.ID, row.AgentID = caseID, agentID
			row, err = repo.UpdateCase(ctx, row)
		}
		h.writeCase(w, row, err, rid)
	default:
		row, err := repo.GetCase(ctx, agentID, caseID)
		h.writeCase(w, row, err, rid)
	}
}

func (h evalHandler) writeCase(w http.ResponseWriter, row storage.EvalCase, err error, rid string) {
	var d EvalCaseDTO
	if err == nil {
		d, err = caseDTO(row)
	}
	if err != nil {
		h.fail(w, err, rid)
		return
	}
	WriteJSON(w, http.StatusOK, SuccessResponse[EvalCaseDTO]{Data: d})
}

// ---- suite and scorer settings

// checkJudge validates a judge to be saved and writes the refusal: an unusable provider is 400, a paid one on a
// process without GOVOICE_LIVE=1 is 403 (the worker enforces the same again, so a judge saved elsewhere never spends).
func (h evalHandler) checkJudge(w http.ResponseWriter, j *EvalJudgeDTO, rid string) bool {
	if j == nil {
		return true
	}
	if j.Provider == "" || len(j.Provider) > evalMaxJudgeField || len(j.Model) > evalMaxJudgeField {
		WriteError(w, http.StatusBadRequest, "invalid_request", "judge.provider is required; provider and model are at most 128 characters", rid)
		return false
	}
	if err := h.policy.JudgeProvider(j.Provider); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid_judge", "judge.provider: "+clip(err.Error(), 200), rid)
		return false
	}
	if h.policy.JudgeLiveRequired(j.Provider) {
		WriteError(w, http.StatusForbidden, "eval_live_required", "a judge on a paid provider needs the server started with GOVOICE_LIVE=1; offline and echo judges need nothing", rid)
		return false
	}
	return true
}

func judgeDTO(provider, model string) *EvalJudgeDTO {
	if provider == "" {
		return nil
	}
	return &EvalJudgeDTO{Provider: provider, Model: model}
}

func (h evalHandler) suite(w http.ResponseWriter, r *http.Request, agentID, rid string) {
	if !methods(w, r, rid, http.MethodGet, http.MethodPut) || !onlyQuery(w, r, rid) {
		return
	}
	repo := h.store.Eval()
	if r.Method == http.MethodPut {
		var req struct {
			Runs          *int          `json:"runs"`
			MinPass       *int          `json:"min_pass"`
			PassThreshold *float64      `json:"pass_threshold"`
			Judge         *EvalJudgeDTO `json:"judge"`
		}
		if !body(w, r, rid, &req, MaxBodyBytes) {
			return
		}
		switch {
		case req.Runs == nil || req.MinPass == nil || req.PassThreshold == nil:
			WriteError(w, http.StatusBadRequest, "invalid_request", "runs, min_pass and pass_threshold are required", rid)
			return
		case *req.Runs < 1 || *req.Runs > 5:
			WriteError(w, http.StatusBadRequest, "invalid_request", "runs must be between 1 and 5", rid)
			return
		case *req.MinPass < 1 || *req.MinPass > *req.Runs:
			WriteError(w, http.StatusBadRequest, "invalid_request", "min_pass must be between 1 and runs", rid)
			return
		case *req.PassThreshold < 0 || *req.PassThreshold > 1:
			WriteError(w, http.StatusBadRequest, "invalid_request", "pass_threshold must be between 0 and 1", rid)
			return
		}
		if !h.checkJudge(w, req.Judge, rid) {
			return
		}
		su := storage.EvalSuite{AgentID: agentID, Runs: *req.Runs, MinPass: *req.MinPass, PassThreshold: *req.PassThreshold}
		if req.Judge != nil {
			su.JudgeProvider, su.JudgeModel = req.Judge.Provider, req.Judge.Model
		}
		if err := repo.PutSuite(r.Context(), su); err != nil {
			h.fail(w, err, rid)
			return
		}
	}
	su, err := repo.Suite(r.Context(), agentID)
	if err != nil {
		h.fail(w, err, rid)
		return
	}
	WriteJSON(w, http.StatusOK, SuccessResponse[EvalSuiteDTO]{Data: EvalSuiteDTO{Runs: su.Runs, MinPass: su.MinPass, PassThreshold: su.PassThreshold, Judge: judgeDTO(su.JudgeProvider, su.JudgeModel)}})
}

func scorerDTO(s eval.Scorer, set *storage.EvalScorerSetting) EvalScorerDTO {
	d := EvalScorerDTO{Name: s.Name(), Judged: s.Judged(), DefaultThreshold: s.DefaultThreshold()}
	if set != nil {
		d.Threshold, d.Judge = set.Threshold, judgeDTO(set.JudgeProvider, set.JudgeModel)
	}
	return d
}

func (h evalHandler) scorers(w http.ResponseWriter, r *http.Request, agentID, rid string) {
	if !methods(w, r, rid, http.MethodGet) || !onlyQuery(w, r, rid) {
		return
	}
	settings, err := h.store.Eval().ScorerSettings(r.Context(), agentID)
	if err != nil {
		h.fail(w, err, rid)
		return
	}
	byName := make(map[string]*storage.EvalScorerSetting, len(settings))
	for i := range settings {
		byName[settings[i].Scorer] = &settings[i]
	}
	all := eval.Builtins()
	out := make([]EvalScorerDTO, 0, len(all))
	for _, s := range all {
		out = append(out, scorerDTO(s, byName[s.Name()]))
	}
	WriteJSON(w, http.StatusOK, SuccessResponse[[]EvalScorerDTO]{Data: out})
}

// putScorer replaces the agent's setting for one scorer: a field left out goes back to the scorer's default.
func (h evalHandler) putScorer(w http.ResponseWriter, r *http.Request, agentID, name, rid string) {
	if !methods(w, r, rid, http.MethodPut) || !onlyQuery(w, r, rid) {
		return
	}
	s, ok := eval.Lookup(name)
	if !ok {
		WriteError(w, http.StatusNotFound, "not_found", "no such scorer", rid)
		return
	}
	var req struct {
		Threshold *float64      `json:"threshold"`
		Judge     *EvalJudgeDTO `json:"judge"`
	}
	if !body(w, r, rid, &req, MaxBodyBytes) {
		return
	}
	switch {
	case req.Threshold != nil && (*req.Threshold < 0 || *req.Threshold > 1):
		WriteError(w, http.StatusBadRequest, "invalid_request", "threshold must be between 0 and 1", rid)
		return
	case req.Judge != nil && !s.Judged():
		WriteError(w, http.StatusBadRequest, "invalid_request", "scorer "+name+" is not model-graded and takes no judge", rid)
		return
	}
	if !h.checkJudge(w, req.Judge, rid) {
		return
	}
	set := storage.EvalScorerSetting{AgentID: agentID, Scorer: name, Threshold: req.Threshold}
	if req.Judge != nil {
		set.JudgeProvider, set.JudgeModel = req.Judge.Provider, req.Judge.Model
	}
	if err := h.store.Eval().PutScorerSetting(r.Context(), set); err != nil {
		h.fail(w, err, rid)
		return
	}
	WriteJSON(w, http.StatusOK, SuccessResponse[EvalScorerDTO]{Data: scorerDTO(s, &set)})
}

// ---- runs

// versionNumbers resolves version ids to their numbers, one lookup per distinct id.
type versionNumbers struct {
	store EvalStore
	seen  map[string]int
}

func (v *versionNumbers) of(ctx context.Context, id string) (*int, error) {
	if id == "" {
		return nil, nil
	}
	if n, ok := v.seen[id]; ok {
		return &n, nil
	}
	ver, err := v.store.Agents().Version(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("load version %s: %w", id, err)
	}
	v.seen[id] = ver.Version
	return &ver.Version, nil
}

func runDTO(ctx context.Context, run storage.EvalRun, versions *versionNumbers) (EvalRunDTO, error) {
	number, err := versions.of(ctx, run.AgentVersionID)
	if err != nil {
		return EvalRunDTO{}, err
	}
	return EvalRunDTO{ID: run.ID, AgentID: run.AgentID, Version: number, AgentVersionID: run.AgentVersionID, Draft: run.Draft,
		ConfigHash: run.ConfigHash, Scope: run.Scope, CaseID: run.CaseID, Providers: run.Providers, Runs: run.Runs, MinPass: run.MinPass, Status: run.Status,
		TriggeredBy: run.TriggeredBy, PassedCases: run.PassedCases, TotalCases: run.TotalCases, CreatedAt: run.CreatedAt, StartedAt: run.StartedAt, EndedAt: run.EndedAt}, nil
}

func (h evalHandler) runs(w http.ResponseWriter, r *http.Request, agentID, rid string) {
	if !methods(w, r, rid, http.MethodGet, http.MethodPost) {
		return
	}
	if r.Method == http.MethodPost {
		h.startRun(w, r, agentID, rid)
		return
	}
	if !onlyQuery(w, r, rid, "limit", "offset", "include_drafts") {
		return
	}
	page, err := ParsePagination(r)
	if err != nil {
		WriteError(w, http.StatusBadRequest, "invalid_request", err.Error(), rid)
		return
	}
	drafts := false
	switch r.URL.Query().Get("include_drafts") {
	case "", "false":
	case "true":
		drafts = true
	default:
		WriteError(w, http.StatusBadRequest, "invalid_request", "include_drafts must be true or false", rid)
		return
	}
	rows, err := h.store.Eval().ListRuns(r.Context(), agentID, drafts, page.Limit, page.Offset)
	if err != nil {
		h.fail(w, err, rid)
		return
	}
	versions := &versionNumbers{store: h.store, seen: map[string]int{}}
	out := make([]EvalRunDTO, 0, len(rows))
	for _, run := range rows {
		dto, err := runDTO(r.Context(), run, versions)
		if err != nil {
			h.fail(w, err, rid)
			return
		}
		out = append(out, dto)
	}
	WriteJSON(w, http.StatusOK, SuccessResponse[[]EvalRunDTO]{Data: out})
}

func (h evalHandler) startRun(w http.ResponseWriter, r *http.Request, agentID, rid string) {
	if !onlyQuery(w, r, rid) {
		return
	}
	var req struct {
		Version   *int          `json:"version"`
		Draft     *DraftRequest `json:"draft"`
		CaseID    string        `json:"case_id"`
		Providers string        `json:"providers"`
	}
	if !body(w, r, rid, &req, MaxBodyBytes) {
		return
	}
	if req.Providers == "" {
		req.Providers = eval.ModeOffline
	}
	switch {
	case req.Providers != eval.ModeKeep && req.Providers != eval.ModeOffline && req.Providers != eval.ModeEcho:
		WriteError(w, http.StatusBadRequest, "invalid_request", "providers must be keep, offline or echo", rid)
		return
	case req.Draft != nil && req.Version != nil && *req.Version != 0:
		WriteError(w, http.StatusBadRequest, "invalid_request", "set version or draft, not both", rid)
		return
	case req.Version != nil && *req.Version < 0:
		WriteError(w, http.StatusBadRequest, "invalid_request", "version must be a version number", rid)
		return
	case req.Draft != nil && len(req.Draft.Config) == 0:
		WriteError(w, http.StatusBadRequest, "invalid_request", "draft.config is required", rid)
		return
	}
	ctx := r.Context()
	ag, err := h.agents.Get(ctx, agentID)
	if err != nil {
		h.fail(w, err, rid)
		return
	}

	// What is tested: a draft, or a published version (the current one unless the request names another).
	run := storage.EvalRun{ID: storage.NewID(), AgentID: agentID, Scope: "suite", CaseID: req.CaseID, Providers: req.Providers, TriggeredBy: "api"}
	var config json.RawMessage
	var versionNumber *int
	readiness := ag.Readiness
	if req.Draft != nil {
		if ag.Archived || !ag.Enabled {
			WriteJSON(w, http.StatusConflict, ErrorResponse{Error: Error{Code: "agent_not_ready", Message: "the agent is not ready to take calls", RequestID: rid, Details: ag.Readiness}})
			return
		}
		// Like a version run, only keep needs the draft's own providers attached and ready: offline and echo are
		// resolved (tools, shape) on the rewritten config, and the run is identified by the config as sent.
		resolve := req.Draft.Config
		if req.Providers != eval.ModeKeep {
			if resolve, err = eval.Rewrite(req.Draft.Config, req.Providers); err != nil {
				WriteError(w, http.StatusBadRequest, "invalid_request", clip(err.Error(), 300), rid)
				return
			}
		}
		snap, rd, err := h.agents.ResolveDraft(ctx, agentID, resolve, req.Draft.MCPTools)
		if err != nil {
			h.draftFail(w, err, rid)
			return
		}
		hash, canonical := snap.ConfigHash, snap.Version.Config
		if req.Providers != eval.ModeKeep {
			if hash, canonical, err = storage.ConfigHash(req.Draft.Config); err != nil {
				WriteError(w, http.StatusBadRequest, "invalid_request", clip(err.Error(), 300), rid)
				return
			}
		}
		readiness, config = rd, canonical
		run.Draft, run.ConfigHash, run.DraftConfigJSON = true, hash, config
		// Always the selection the service checked: without mcp_tools it is the base version's, and an empty
		// stored value would reach the worker as "no tools".
		if run.DraftToolsJSON, err = json.Marshal(snap.Version.MCPTools); err != nil {
			h.fail(w, fmt.Errorf("encode draft tools: %w", err), rid)
			return
		}
	} else {
		ver := ag.Version
		if req.Version != nil && *req.Version != 0 {
			v, err := h.agents.VersionByNumber(ctx, agentID, *req.Version)
			if err != nil {
				h.fail(w, err, rid)
				return
			}
			ver = &v
		}
		if ver == nil {
			WriteJSON(w, http.StatusConflict, ErrorResponse{Error: Error{Code: "agent_not_ready", Message: "the agent has no published version", RequestID: rid, Details: ag.Readiness}})
			return
		}
		config, versionNumber = ver.Config, &ver.Number
		run.AgentVersionID, run.ConfigHash = ver.ID, ver.ConfigHash
	}
	// A version run rewritten to offline or echo does not use the providers the current readiness is about (their keys
	// may be absent), so only keep needs it (a draft's readiness is its own).
	needReady := req.Providers == eval.ModeKeep
	if ag.Archived || !ag.Enabled || (needReady && !agent.Callable(readiness.State)) {
		WriteJSON(w, http.StatusConflict, ErrorResponse{Error: Error{Code: "agent_not_ready", Message: "the agent is not ready to take calls", RequestID: rid, Details: readiness}})
		return
	}

	// The paid gate: the rewrite is what the call runs, so it decides whether a paid model is reached.
	rewritten, err := eval.Rewrite(config, req.Providers)
	if err != nil {
		WriteError(w, http.StatusBadRequest, "invalid_request", clip(err.Error(), 300), rid)
		return
	}
	if h.policy.LiveRequired(rewritten) {
		WriteError(w, http.StatusForbidden, "eval_live_required", "this run reaches a paid model; start the server with GOVOICE_LIVE=1, or run with providers offline or echo", rid)
		return
	}

	// The cases, snapshotted into the jobs so a later edit does not change a queued run.
	repo := h.store.Eval()
	var rows []storage.EvalCase
	if req.CaseID != "" {
		run.Scope = "case"
		row, err := repo.GetCase(ctx, agentID, req.CaseID)
		if err != nil {
			h.fail(w, err, rid)
			return
		}
		rows = []storage.EvalCase{row}
	} else if rows, err = repo.ListCases(ctx, agentID); err != nil {
		h.fail(w, err, rid)
		return
	}
	suite, err := repo.Suite(ctx, agentID)
	if err != nil {
		h.fail(w, err, rid)
		return
	}
	run.Runs, run.MinPass = suite.Runs, suite.MinPass
	if len(rows)*suite.Runs > storage.MaxPendingJobsPerAgent {
		WriteError(w, http.StatusConflict, "suite_too_large", fmt.Sprintf("cases x runs exceeds %d, the most one agent can have queued; remove cases or lower the suite's runs", storage.MaxPendingJobsPerAgent), rid)
		return
	}
	for _, row := range rows {
		if c, err := caseFromRow(row); err == nil && c.Caller.Persona != nil && h.policy.PersonaLiveRequired() {
			WriteError(w, http.StatusForbidden, "eval_live_required", "a persona caller asks the offline provider's model, which is a remote endpoint; start the server with GOVOICE_LIVE=1", rid)
			return
		}
	}
	if r.Header.Get("X-Switchboard-UI") == "1" {
		run.TriggeredBy = "console"
	}
	jobs := make([]storage.EvalJob, 0, len(rows)*suite.Runs)
	for _, row := range rows {
		c, err := caseFromRow(row)
		if err == nil {
			var hash string
			if hash, err = caseHash(c); err == nil {
				for repeat := range suite.Runs {
					var payload []byte
					if payload, err = json.Marshal(eval.CaseJob{CaseID: row.ID, CaseHash: hash, Case: c, Repeat: repeat}); err != nil {
						break
					}
					subject := row.ID + "#" + strconv.Itoa(repeat)
					jobs = append(jobs, storage.EvalJob{Kind: "run_case", Subject: subject, IdempotencyKey: "run:" + run.ID + ":" + subject, MaxAttempts: 3, Payload: payload})
				}
			}
		}
		if err != nil {
			h.fail(w, err, rid)
			return
		}
	}
	created, err := repo.CreateRun(ctx, run, jobs) // ErrNoJobs: an empty suite
	if err != nil {
		h.fail(w, err, rid)
		return
	}
	dto, err := runDTO(ctx, created, &versionNumbers{store: h.store, seen: map[string]int{}})
	if err != nil {
		h.fail(w, err, rid)
		return
	}
	dto.Version = versionNumber
	WriteJSON(w, http.StatusAccepted, SuccessResponse[EvalRunDTO]{Data: dto})
}

// draftFail maps a draft the agent service refused, as the voice-token endpoint does.
func (h evalHandler) draftFail(w http.ResponseWriter, err error, rid string) {
	switch {
	case errors.Is(err, sql.ErrNoRows): // the agent has no published version to base a draft on
		WriteError(w, http.StatusConflict, "agent_not_ready", "the agent is not ready to take calls", rid)
	case errors.Is(err, agent.ErrNotAttached):
		WriteError(w, http.StatusBadRequest, "agent_not_attached", err.Error(), rid)
	case errors.Is(err, agent.ErrInvalidConfig), errors.Is(err, storage.ErrInvalidToolSelection):
		WriteError(w, http.StatusBadRequest, "invalid_request", clip(err.Error(), 500), rid)
	default:
		h.fail(w, err, rid)
	}
}

func (h evalHandler) runDetail(w http.ResponseWriter, r *http.Request, agentID, runID, rid string) {
	if !methods(w, r, rid, http.MethodGet) || !onlyQuery(w, r, rid) {
		return
	}
	ctx, repo := r.Context(), h.store.Eval()
	run, err := repo.GetRun(ctx, agentID, runID)
	var jobs []storage.EvalJob
	if err == nil {
		jobs, err = repo.RunJobs(ctx, runID)
	}
	var results []storage.EvalResult
	if err == nil {
		results, err = repo.RunResults(ctx, runID)
	}
	if err != nil {
		h.fail(w, err, rid)
		return
	}
	scores := make(map[string][]EvalScoreDTO, len(jobs))
	sessions := make(map[string]string, len(jobs))
	failed := make(map[string]bool, len(jobs))
	for _, res := range results {
		scores[res.SubjectID] = append(scores[res.SubjectID], EvalScoreDTO{Scorer: res.Scorer, Score: res.Score, Outcome: res.Outcome, Reason: clip(res.Reason, evalMaxReason), JudgeModel: res.JudgeModel})
		sessions[res.SubjectID] = res.SessionID
		failed[res.SubjectID] = failed[res.SubjectID] || res.Outcome == string(eval.Fail) || res.Outcome == string(eval.Errored)
	}
	out := make([]EvalCaseRunDTO, 0, len(jobs))
	for _, j := range jobs {
		var cj eval.CaseJob
		_ = json.Unmarshal(j.Payload, &cj) // a payload this handler wrote; a damaged one still lists, nameless
		subject := storage.EvalCaseRunSubject(runID, j.Subject)
		ss := scores[subject]
		if ss == nil {
			ss = []EvalScoreDTO{}
		}
		out = append(out, EvalCaseRunDTO{CaseID: cj.CaseID, CaseName: cj.Case.Name, Repeat: cj.Repeat, Status: j.Status, ErrorCategory: j.LastErrorCategory,
			SessionID: sessions[subject], Passed: j.Status == "done" && !failed[subject], Scores: ss})
	}
	runOut, err := runDTO(ctx, run, &versionNumbers{store: h.store, seen: map[string]int{}})
	if err != nil {
		h.fail(w, err, rid)
		return
	}
	WriteJSON(w, http.StatusOK, SuccessResponse[EvalRunDetailDTO]{Data: EvalRunDetailDTO{Run: runOut, CaseRuns: out}})
}
