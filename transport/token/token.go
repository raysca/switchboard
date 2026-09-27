package token

import (
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var (
	ErrTokenMissing     = errors.New("token: admission token missing")
	ErrTokenInvalid     = errors.New("token: admission token invalid or malformed")
	ErrTokenExpired     = errors.New("token: admission token expired")
	ErrTokenReplayed    = errors.New("token: admission token has already been consumed")
	ErrTokenIssuer      = errors.New("token: invalid token issuer")
	ErrTokenAudience    = errors.New("token: invalid token audience")
	ErrTokenAlgorithm   = errors.New("token: disallowed signature algorithm")
	ErrTokenLifetime    = errors.New("token: token lifetime exceeds maximum allowed")
	ErrTokenBeforeStart = errors.New("token: token issued before server start boundary")
	ErrTokenFuture      = errors.New("token: token issued in the future beyond allowed skew")
)

var tokenQueryRegex = regexp.MustCompile(`([?&]token=)[^&#\s]+`)

// RedactToken replaces token parameter values in URLs or logs with [REDACTED].
func RedactToken(s string) string {
	return tokenQueryRegex.ReplaceAllString(s, "${1}[REDACTED]")
}

// Config defines the validation parameters for admission tokens.
type Config struct {
	Issuer            string
	Audience          string
	AllowedAlgorithms []string
	MaxLifetime       time.Duration // Maximum acceptable exp - iat duration (e.g. 60s)
	ClockSkew         time.Duration // Acceptable clock skew allowance (default: 5s)
	ReplayCacheSize   int           // Max entries before aggressive pruning
	HMACSecret        []byte        // Secret key for HMAC algorithms (e.g. HS256)
}

// DefaultConfig provides production-grade defaults.
func DefaultConfig(issuer, audience string, secret []byte) Config {
	return Config{
		Issuer:            issuer,
		Audience:          audience,
		AllowedAlgorithms: []string{"HS256"},
		MaxLifetime:       60 * time.Second,
		ClockSkew:         5 * time.Second,
		ReplayCacheSize:   10000,
		HMACSecret:        secret,
	}
}

// Claims represents the standard and custom claims expected in V1 tokens.
type Claims struct {
	jwt.RegisteredClaims
	Principal string `json:"principal,omitempty"`
}

// Validator manages admission token verification, atomic single-use consumption,
// clock-skew enforcement, and post-restart iat gating.
type Validator struct {
	cfg       Config
	startTime time.Time

	mu     sync.Mutex
	replay map[string]time.Time // jti -> exp
}

// NewValidator initializes a Validator with the given configuration and records process start time.
func NewValidator(cfg Config, startTime time.Time) (*Validator, error) {
	if cfg.ClockSkew <= 0 {
		cfg.ClockSkew = 5 * time.Second
	}
	if cfg.MaxLifetime <= 0 {
		cfg.MaxLifetime = 60 * time.Second
	}
	if cfg.ReplayCacheSize <= 0 {
		cfg.ReplayCacheSize = 10000
	}
	if startTime.IsZero() {
		startTime = time.Now()
	}

	return &Validator{
		cfg:       cfg,
		startTime: startTime,
		replay:    make(map[string]time.Time),
	}, nil
}

// ValidateAndConsume validates the token string and atomically marks its JTI as consumed.
// If valid and unused, it returns the parsed Claims; otherwise it returns an explicit error.
func (v *Validator) ValidateAndConsume(tokenStr string) (*Claims, error) {
	if tokenStr == "" {
		return nil, ErrTokenMissing
	}

	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (interface{}, error) {
		alg := t.Method.Alg()
		allowed := false
		for _, a := range v.cfg.AllowedAlgorithms {
			if a == alg {
				allowed = true
				break
			}
		}
		if !allowed {
			return nil, fmt.Errorf("%w: %s", ErrTokenAlgorithm, alg)
		}
		return v.cfg.HMACSecret, nil
	}, jwt.WithAudience(v.cfg.Audience), jwt.WithIssuer(v.cfg.Issuer))

	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrTokenExpired
		}
		if errors.Is(err, jwt.ErrTokenInvalidAudience) {
			return nil, ErrTokenAudience
		}
		if errors.Is(err, jwt.ErrTokenInvalidIssuer) {
			return nil, ErrTokenIssuer
		}
		if errors.Is(err, ErrTokenAlgorithm) {
			return nil, ErrTokenAlgorithm
		}
		return nil, fmt.Errorf("%w: %v", ErrTokenInvalid, err)
	}

	if !token.Valid {
		return nil, ErrTokenInvalid
	}

	if claims.ID == "" {
		return nil, fmt.Errorf("%w: missing jti claim", ErrTokenInvalid)
	}

	if claims.IssuedAt == nil {
		return nil, fmt.Errorf("%w: missing iat claim", ErrTokenInvalid)
	}
	if claims.ExpiresAt == nil {
		return nil, fmt.Errorf("%w: missing exp claim", ErrTokenInvalid)
	}

	iat := claims.IssuedAt.Time
	exp := claims.ExpiresAt.Time
	now := time.Now()

	// Clock skew future check
	if iat.After(now.Add(v.cfg.ClockSkew)) {
		return nil, ErrTokenFuture
	}

	// Post-restart iat rejection rule:
	// Losing in-memory replay cache across restarts must not allow replaying older tokens.
	if iat.Before(v.startTime.Add(-v.cfg.ClockSkew)) {
		return nil, ErrTokenBeforeStart
	}

	// Maximum token lifetime enforcement
	if exp.Sub(iat) > v.cfg.MaxLifetime {
		return nil, ErrTokenLifetime
	}

	// Atomic replay consumption check
	v.mu.Lock()
	defer v.mu.Unlock()

	// Prune expired entries if cache is large
	if len(v.replay) >= v.cfg.ReplayCacheSize {
		for id, expiry := range v.replay {
			if now.After(expiry) {
				delete(v.replay, id)
			}
		}
	}

	if _, exists := v.replay[claims.ID]; exists {
		return nil, ErrTokenReplayed
	}

	v.replay[claims.ID] = exp
	return claims, nil
}
