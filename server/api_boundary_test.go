package server

import (
	"encoding/json"
	"errors"
	"govoice/server/agent"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"govoice/transport/token"
)

func apiTestServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv("GOVOICE_MANAGEMENT_API_KEY", "management-test-key-32-bytes-long-and-separate")
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Token.Secret = "voice-test-secret-32-bytes-long-and-separate"
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.env = testProviderEnv(t)
	return s
}

// testProviderEnv is a map-backed environment with google and xai credentials.
func testProviderEnv(t *testing.T) func(string) string {
	t.Helper()
	return mapEnv(testProviderVars(t))
}

// testProviderVars are the google and xai variables of testProviderEnv.
func testProviderVars(t *testing.T) map[string]string {
	t.Helper()
	sa := filepath.Join(t.TempDir(), "sa.json")
	if err := os.WriteFile(sa, []byte(`{"type":"service_account"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return map[string]string{"GOOGLE_API_KEY": "test-google-api-key", "GOOGLE_APPLICATION_CREDENTIALS": sa, "XAI_API_KEY": "test-xai-key"}
}

func apiRequest(s *Server, method, path, bearer, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	s.adminMux.ServeHTTP(w, r)
	return w
}

func TestAPIAuthRouteSeparationAndEnvelope(t *testing.T) {
	s := apiTestServer(t)
	key := "management-test-key-32-bytes-long-and-separate"
	w := apiRequest(s, http.MethodGet, "/api/v1/server-config", "", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d", w.Code)
	}
	var failure struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &failure); err != nil || failure.Error.Code == "" || failure.Error.RequestID == "" {
		t.Fatalf("bad envelope: %s %v", w.Body.String(), err)
	}
	if w.Header().Get("X-Request-ID") != failure.Error.RequestID {
		t.Fatal("request ID mismatch")
	}
	if w := apiRequest(s, http.MethodGet, "/api/v1/server-config", "wrong-key-sentinel", ""); strings.Contains(w.Body.String(), "wrong-key-sentinel") {
		t.Fatal("unauthorized response echoed credential")
	}
	if w := apiRequest(s, http.MethodGet, "/api/v1/server-config", s.cfg.Token.Secret, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("voice secret admitted: %d", w.Code)
	}
	if w := apiRequest(s, http.MethodGet, "/api/v1/server-config", key, ""); w.Code != http.StatusOK {
		t.Fatalf("valid key status=%d body=%s", w.Code, w.Body.String())
	}
	pub := httptest.NewRecorder()
	s.publicMux.ServeHTTP(pub, httptest.NewRequest("GET", "/api/v1/server-config", nil))
	if pub.Code != http.StatusNotFound {
		t.Fatalf("management route exposed publicly: %d", pub.Code)
	}
	if w := apiRequest(s, http.MethodDelete, "/api/v1/server-config", key, ""); w.Code != http.StatusMethodNotAllowed || !strings.Contains(w.Body.String(), `"error"`) {
		t.Fatalf("method error: %d %s", w.Code, w.Body.String())
	}
}

func TestAPITokenExpiryScopeReplay(t *testing.T) {
	s := apiTestServer(t)
	key := "management-test-key-32-bytes-long-and-separate"
	agentID := readyTestAgent(t, s)
	w := apiRequest(s, http.MethodPost, "/api/v1/voice-tokens", key, `{"principal":"alice","agent_id":"`+agentID+`"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("issue status=%d body=%s", w.Code, w.Body.String())
	}
	var response struct {
		Data struct {
			Token     string    `json:"token"`
			ExpiresAt time.Time `json:"expires_at"`
			Scope     string    `json:"scope"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.Scope != "voice:connect" || response.Data.ExpiresAt.After(time.Now().Add(61*time.Second)) {
		t.Fatalf("bad scope/lifetime: %+v", response.Data)
	}
	claims, err := s.validator.ValidateAndConsume(response.Data.Token)
	if err != nil || claims.Principal != "alice" || claims.Scope != "voice:connect" {
		t.Fatalf("invalid issued token: %+v %v", claims, err)
	}
	if !response.Data.ExpiresAt.Equal(claims.ExpiresAt.Time) {
		t.Fatalf("reported expiry %s differs from JWT expiry %s", response.Data.ExpiresAt, claims.ExpiresAt.Time)
	}
	if _, err := s.validator.ValidateAndConsume(response.Data.Token); err != token.ErrTokenReplayed {
		t.Fatalf("replay: %v", err)
	}
	restarted := apiTestServer(t)
	if _, err := restarted.validator.ValidateAndConsume(response.Data.Token); err == nil {
		t.Fatal("restarted server accepted prior-process scoped token")
	}
	if w := apiRequest(s, http.MethodGet, "/api/v1/server-config", response.Data.Token, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("voice token admitted to management: %d", w.Code)
	}
	for _, tc := range []struct {
		name, scope string
		expiry      time.Time
	}{
		{"wrong scope", "management:read", time.Now().Add(time.Minute)},
		{"expired", "voice:connect", time.Now().Add(-time.Minute)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issued := time.Now()
			if tc.name == "expired" {
				issued = time.Now().Add(-2 * time.Minute)
			}
			claims := token.Claims{RegisteredClaims: jwt.RegisteredClaims{Issuer: s.cfg.Token.Issuer, Audience: jwt.ClaimStrings{s.cfg.Token.Audience}, Subject: "alice", ID: tc.name, IssuedAt: jwt.NewNumericDate(issued), ExpiresAt: jwt.NewNumericDate(tc.expiry)}, Scope: tc.scope}
			jwtToken := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
			jwtToken.Header["kid"] = s.cfg.Token.KeyID
			signed, err := jwtToken.SignedString(s.voiceSignKey)
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.validator.ValidateAndConsume(signed)
			if err == nil {
				t.Fatal("accepted invalid voice token")
			}
			if tc.name == "expired" && !errors.Is(err, token.ErrTokenExpired) {
				t.Fatalf("expiry error=%v", err)
			}
			if tc.name == "wrong scope" && !errors.Is(err, token.ErrTokenInvalid) {
				t.Fatalf("scope error=%v", err)
			}
		})
	}
}

func TestAPITokenProductionDefaultKeyID(t *testing.T) {
	s := apiTestServer(t)
	s.cfg.Profile = ProfileProduction
	s.cfg.Token.KeyID = ""
	configured := voiceTokenConfig(s.cfg, string(s.voiceSignKey))
	var err error
	s.validator, err = token.NewValidator(configured, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	s.voiceKeyID = configured.KeyID
	s.voiceEpoch = s.validator.ScopeEpoch()
	agentID := readyTestAgent(t, s)
	w := apiRequest(s, "POST", "/api/v1/voice-tokens", "management-test-key-32-bytes-long-and-separate", `{"principal":"alice","agent_id":"`+agentID+`"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var response struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if _, err := s.validator.ValidateAndConsume(response.Data.Token); err != nil {
		t.Fatalf("minted production token rejected: %v", err)
	}
}

func TestAPIHandlerValidationAndCanonicalErrors(t *testing.T) {
	s := apiTestServer(t)
	key := "management-test-key-32-bytes-long-and-separate"
	for _, tc := range []struct {
		name, path, contentType, body string
		want                          int
	}{
		{"missing content type", "/api/v1/voice-tokens", "", `{"principal":"alice","agent_id":"a"}`, 400},
		{"malformed", "/api/v1/voice-tokens", "application/json", `{`, 400},
		{"trailing", "/api/v1/voice-tokens", "application/json", `{"principal":"alice","pipeline":"default"} {}`, 400},
		{"oversized whitespace", "/api/v1/voice-tokens", "application/json", `{"principal":"alice","agent_id":"a"}` + strings.Repeat(" ", 65536), 400},
		{"hidden second value", "/api/v1/voice-tokens", "application/json", `{"principal":"alice","agent_id":"a"}` + strings.Repeat(" ", 65536) + `{}`, 400},
		{"bare version", "/api/v1", "", "", 404},
		{"double slash", "/api/v1//server-config", "", "", 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body))
			if tc.name == "bare version" || tc.name == "double slash" {
				r.Method = "GET"
			}
			r.Header.Set("Authorization", "Bearer "+key)
			if tc.contentType != "" {
				r.Header.Set("Content-Type", tc.contentType)
			}
			w := httptest.NewRecorder()
			s.adminMux.ServeHTTP(w, r)
			if w.Code != tc.want || w.Header().Get("X-Request-ID") == "" || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), `"error"`) {
				t.Fatalf("status=%d headers=%v body=%s", w.Code, w.Header(), w.Body.String())
			}
			var failure struct {
				Error struct {
					Code      string `json:"code"`
					RequestID string `json:"request_id"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &failure); err != nil || failure.Error.Code == "" || failure.Error.RequestID != w.Header().Get("X-Request-ID") {
				t.Fatalf("invalid error envelope: %s %v", w.Body.String(), err)
			}
		})
	}
}

func TestConfigRejectsSubsecondVoiceLifetime(t *testing.T) {
	for _, lifetime := range []time.Duration{500 * time.Millisecond, -time.Second} {
		cfg := DefaultConfig(ProfileDevelopment)
		cfg.Token.MaxLifetime = lifetime
		if err := ValidateConfig(cfg); err == nil {
			t.Fatalf("accepted invalid token lifetime %s", lifetime)
		}
	}
}

func TestAPITokenFractionalLifetimeIsBounded(t *testing.T) {
	s := apiTestServer(t)
	s.cfg.Token.MaxLifetime = 2500 * time.Millisecond
	agentID := readyTestAgent(t, s)
	w := apiRequest(s, "POST", "/api/v1/voice-tokens", "management-test-key-32-bytes-long-and-separate", `{"principal":"alice","agent_id":"`+agentID+`"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var response struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	claims, err := s.validator.ValidateAndConsume(response.Data.Token)
	if err != nil {
		t.Fatal(err)
	}
	if got := claims.ExpiresAt.Time.Sub(claims.IssuedAt.Time); got > 2500*time.Millisecond || got < time.Second {
		t.Fatalf("signed lifetime=%s", got)
	}
}

func TestAuthManagementUnavailableAndSeparateKey(t *testing.T) {
	t.Setenv("GOVOICE_MANAGEMENT_API_KEY", "")
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Token.Secret = "voice-secret-32-characters-long-123456"
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if w := apiRequest(s, "GET", "/api/v1/server-config", "anything", ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured status=%d", w.Code)
	}
	t.Setenv("GOVOICE_MANAGEMENT_API_KEY", cfg.Token.Secret)
	if _, err := NewServer(cfg); err == nil {
		t.Fatal("accepted identical management and voice secrets")
	}
}

func TestAuthShortManagementKeyRejected(t *testing.T) {
	t.Setenv("GOVOICE_MANAGEMENT_API_KEY", "too-short-secret")
	cfg := DefaultConfig(ProfileDevelopment)
	_, err := NewServer(cfg)
	if err == nil || !strings.Contains(err.Error(), "at least 32 bytes") || strings.Contains(err.Error(), "too-short-secret") {
		t.Fatalf("short management key error=%v", err)
	}
}

func TestAuthDefaultEnvironmentForLoadedConfig(t *testing.T) {
	t.Setenv("GOVOICE_MANAGEMENT_API_KEY", "management-test-key-32-bytes-long-and-separate")
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Management.APIKeyEnv = "" // YAML without a management block
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if w := apiRequest(s, "GET", "/api/v1/server-config", "management-test-key-32-bytes-long-and-separate", ""); w.Code != http.StatusOK {
		t.Fatalf("default env status=%d", w.Code)
	}
}

func TestAPIConfigSecretRedactionAndJSONErrors(t *testing.T) {
	s := apiTestServer(t)
	w := apiRequest(s, "GET", "/api/v1/server-config", "management-test-key-32-bytes-long-and-separate", "")
	for _, secret := range []string{s.cfg.Token.Secret, "management-test-key-32-bytes-long-and-separate"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("config leaked secret: %s", secret)
		}
	}
	w = apiRequest(s, "POST", "/api/v1/voice-tokens", "management-test-key-32-bytes-long-and-separate", `{"principal":"alice","unexpected":1}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"code":"invalid_request"`) {
		t.Fatalf("invalid JSON: %d %s", w.Code, w.Body.String())
	}
}

// Voice tokens name a stored agent; unknown, unready and legacy requests are refused.
func TestAPITokenRequiresReadyStoredAgent(t *testing.T) {
	s := apiTestServer(t)
	key := "management-test-key-32-bytes-long-and-separate"
	issue := func(body string) *httptest.ResponseRecorder {
		return apiRequest(s, http.MethodPost, "/api/v1/voice-tokens", key, body)
	}
	if w := issue(`{"principal":"alice","agent_id":"x"}`); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("without agent store: %d %s", w.Code, w.Body)
	}
	agentID := readyTestAgent(t, s)
	if w := issue(`{"principal":"alice","pipeline":"default"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("legacy pipeline body: %d %s", w.Code, w.Body)
	}
	if w := issue(`{"principal":"alice","agent_id":"missing"}`); w.Code != http.StatusNotFound {
		t.Fatalf("unknown agent: %d %s", w.Code, w.Body)
	}
	disabled := false
	if _, err := s.agents.Update(t.Context(), agentID, agent.UpdateRequest{Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	if w := issue(`{"principal":"alice","agent_id":"` + agentID + `"}`); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "agent_not_ready") {
		t.Fatalf("disabled agent: %d %s", w.Code, w.Body)
	}
	enabled := true
	if _, err := s.agents.Update(t.Context(), agentID, agent.UpdateRequest{Enabled: &enabled}); err != nil {
		t.Fatal(err)
	}
	w := issue(`{"principal":"alice","agent_id":"` + agentID + `"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("ready agent: %d %s", w.Code, w.Body)
	}
	var resp struct {
		Data struct{ Token string } `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	claims, err := s.validator.ValidateAndConsume(resp.Data.Token)
	if err != nil || claims.AgentID != agentID {
		t.Fatalf("token claims agent=%q err=%v", claims.AgentID, err)
	}
}
