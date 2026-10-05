// Package credential manages encrypted provider secrets. Plaintext is only
// transiently available inside the service during validation and rotation.
package credential

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"govoice/server/provider"
	"govoice/server/storage"
)

const MaxSecretBytes = 8192

const (
	ValidationUnvalidated = "unvalidated"
	ValidationValid       = "valid"
	ValidationInvalid     = "invalid"
	ValidationError       = "error"
)

var (
	ErrConfiguration         = errors.New("credential encryption configuration is invalid")
	ErrInvalidInput          = errors.New("credential input is invalid")
	ErrInvalidCredential     = errors.New("provider rejected credential")
	ErrValidationUnavailable = errors.New("provider validation is unavailable")
	ErrNotFound              = errors.New("credential not found")
	ErrDecrypt               = errors.New("credential ciphertext authentication failed")
	ErrUnknownKey            = errors.New("credential encryption key is unavailable")
	ErrUnsupportedProvider   = errors.New("provider is not supported")
	errValidatorFailed       = errors.New("provider validation failed")
)

type Keyring struct {
	ActiveID string
	Keys     map[string][]byte
}

type CredentialDTO struct {
	ID              string                `json:"id"`
	Provider        string                `json:"provider"`
	MaskedValue     string                `json:"masked_value"`
	ValidationState string                `json:"validation_state"`
	ValidatedAt     *time.Time            `json:"validated_at,omitempty"`
	Capabilities    []provider.Capability `json:"capabilities"`
	CreatedAt       time.Time             `json:"created_at"`
	UpdatedAt       time.Time             `json:"updated_at"`
}

func (d CredentialDTO) String() string {
	return fmt.Sprintf("CredentialDTO{ID:%s Provider:%s MaskedValue:%s ValidationState:%s}", d.ID, d.Provider, d.MaskedValue, d.ValidationState)
}

type CreateInput struct{ Provider, Secret string }
type UpdateInput struct{ Secret string }
type ValidationResult struct{ Capabilities []provider.Capability }
type Validator interface {
	Validate(context.Context, string, []byte) (ValidationResult, error)
}
type ValidatorFunc func(context.Context, string, []byte) (ValidationResult, error)

func (f ValidatorFunc) Validate(ctx context.Context, providerID string, secret []byte) (ValidationResult, error) {
	return f(ctx, providerID, secret)
}

type Repository interface {
	Put(context.Context, storage.Credential) error
	ReplaceIfUnchanged(context.Context, string, []byte, storage.Credential) error
	RotateIfUnchanged(context.Context, string, string, []byte, string, []byte, string) error
	Get(context.Context, string) (storage.Credential, error)
	Delete(context.Context, string) error
	List(context.Context) ([]storage.Credential, error)
}

type Service struct {
	repo      Repository
	keys      Keyring
	registry  *provider.Registry
	validator Validator
}

func NewService(repo Repository, keys Keyring, registry *provider.Registry, validator Validator) (*Service, error) {
	if repo == nil || registry == nil || keys.ActiveID == "" || len(keys.Keys[keys.ActiveID]) != 32 {
		return nil, ErrConfiguration
	}
	cloned := Keyring{ActiveID: keys.ActiveID, Keys: make(map[string][]byte, len(keys.Keys))}
	for id, key := range keys.Keys {
		if id == "" || len(key) != 32 {
			return nil, ErrConfiguration
		}
		cloned.Keys[id] = append([]byte(nil), key...)
	}
	return &Service{repo: repo, keys: cloned, registry: registry, validator: validator}, nil
}

func (s *Service) Create(ctx context.Context, input CreateInput) (CredentialDTO, error) {
	providerID := strings.ToLower(strings.TrimSpace(input.Provider))
	if _, ok := s.registry.Provider(providerID); !ok {
		return CredentialDTO{}, ErrUnsupportedProvider
	}
	if err := validateSecret(input.Secret); err != nil {
		return CredentialDTO{}, err
	}
	id := storage.NewID()
	keyID := s.keys.ActiveID
	plaintext := []byte(input.Secret)
	defer zero(plaintext)
	ciphertext, err := s.encrypt(id, providerID, keyID, plaintext)
	if err != nil {
		return CredentialDTO{}, err
	}
	capabilities, _ := json.Marshal([]provider.Capability{})
	row := storage.Credential{ID: id, Provider: providerID, MaskedSuffix: mask(input.Secret), ValidationState: ValidationUnvalidated, CapabilitiesJSON: string(capabilities), Ciphertext: ciphertext, KeyID: keyID}
	row.CreatedAt = time.Now().UTC()
	row.UpdatedAt = row.CreatedAt
	if err := s.repo.Put(ctx, row); err != nil {
		return CredentialDTO{}, errors.New("credential could not be stored")
	}
	persisted, err := s.repo.Get(ctx, id)
	if err != nil {
		return CredentialDTO{}, errors.New("credential could not be read after storage")
	}
	return toDTO(persisted), nil
}

func (s *Service) Update(ctx context.Context, id string, input UpdateInput) (CredentialDTO, error) {
	if err := validateSecret(input.Secret); err != nil {
		return CredentialDTO{}, err
	}
	row, err := s.repo.Get(ctx, id)
	if err != nil {
		return CredentialDTO{}, ErrNotFound
	}
	oldKeyID := row.KeyID
	oldCiphertext := append([]byte(nil), row.Ciphertext...)
	row.KeyID = s.keys.ActiveID
	plaintext := []byte(input.Secret)
	defer zero(plaintext)
	row.Ciphertext, err = s.encrypt(row.ID, row.Provider, row.KeyID, plaintext)
	if err != nil {
		return CredentialDTO{}, err
	}
	row.MaskedSuffix = mask(input.Secret)
	row.ValidationState = ValidationUnvalidated
	row.ValidatedAt = time.Time{}
	row.CapabilitiesJSON = "[]"
	if err := s.repo.ReplaceIfUnchanged(ctx, oldKeyID, oldCiphertext, row); err != nil {
		return CredentialDTO{}, errors.New("credential could not be updated")
	}
	persisted, err := s.repo.Get(ctx, id)
	if err != nil {
		return CredentialDTO{}, errors.New("credential could not be read after update")
	}
	return toDTO(persisted), nil
}

func (s *Service) Delete(ctx context.Context, id string) error {
	if err := s.repo.Delete(ctx, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrNotFound) {
			return ErrNotFound
		}
		return errors.New("credential could not be deleted")
	}
	return nil
}

func (s *Service) Get(ctx context.Context, id string) (CredentialDTO, error) {
	row, err := s.repo.Get(ctx, id)
	if err != nil {
		return CredentialDTO{}, ErrNotFound
	}
	return toDTO(row), nil
}

func (s *Service) List(ctx context.Context) ([]CredentialDTO, error) {
	rows, err := s.repo.List(ctx)
	if err != nil {
		return nil, errors.New("credentials could not be listed")
	}
	out := make([]CredentialDTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDTO(row))
	}
	return out, nil
}

func (s *Service) Validate(ctx context.Context, id string) (CredentialDTO, error) {
	row, err := s.repo.Get(ctx, id)
	if err != nil {
		return CredentialDTO{}, ErrNotFound
	}
	if s.validator == nil {
		return CredentialDTO{}, ErrValidationUnavailable
	}
	secret, err := s.decrypt(row)
	if err != nil {
		return CredentialDTO{}, err
	}
	defer zero(secret)
	result, validationErr := invokeValidator(ctx, s.validator, row.Provider, secret)
	row.ValidatedAt = time.Now().UTC()
	switch {
	case errors.Is(validationErr, ErrInvalidCredential):
		row.ValidationState = ValidationInvalid
		row.CapabilitiesJSON = "[]"
	case validationErr != nil:
		row.ValidationState = ValidationError
		row.CapabilitiesJSON = "[]"
	case !validCapabilities(s.registry, row.Provider, result.Capabilities):
		row.ValidationState = ValidationError
		row.CapabilitiesJSON = "[]"
	default:
		row.ValidationState = ValidationValid
		encoded, _ := json.Marshal(sortedCapabilities(result.Capabilities))
		row.CapabilitiesJSON = string(encoded)
	}
	if err := s.repo.ReplaceIfUnchanged(ctx, row.KeyID, row.Ciphertext, row); err != nil {
		return CredentialDTO{}, errors.New("credential validation state could not be stored")
	}
	persisted, err := s.repo.Get(ctx, id)
	if err != nil {
		return CredentialDTO{}, errors.New("credential validation state could not be read")
	}
	return toDTO(persisted), nil
}

func (s *Service) Rotate(ctx context.Context, id string) error {
	row, err := s.repo.Get(ctx, id)
	if err != nil {
		return ErrNotFound
	}
	if row.KeyID == s.keys.ActiveID {
		return nil
	}
	secret, err := s.decrypt(row)
	if err != nil {
		return err
	}
	defer zero(secret)
	oldKeyID := row.KeyID
	oldCiphertext := append([]byte(nil), row.Ciphertext...)
	newCiphertext, err := s.encrypt(row.ID, row.Provider, s.keys.ActiveID, secret)
	if err != nil {
		return err
	}
	return s.repo.RotateIfUnchanged(ctx, row.ID, oldKeyID, oldCiphertext, s.keys.ActiveID, newCiphertext, maskBytes(secret))
}

func invokeValidator(ctx context.Context, validator Validator, providerID string, secret []byte) (result ValidationResult, err error) {
	defer func() {
		if recover() != nil {
			result = ValidationResult{}
			err = errValidatorFailed
		}
	}()
	return validator.Validate(ctx, providerID, secret)
}

func (s *Service) encrypt(id, providerID, keyID string, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(s.keys.Keys[keyID])
	if err != nil {
		return nil, ErrConfiguration
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, ErrConfiguration
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, errors.New("credential encryption unavailable")
	}
	return aead.Seal(nonce, nonce, plaintext, associatedData(id, providerID, keyID)), nil
}

func (s *Service) decrypt(row storage.Credential) ([]byte, error) {
	key, ok := s.keys.Keys[row.KeyID]
	if !ok {
		return nil, ErrUnknownKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrConfiguration
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, ErrConfiguration
	}
	if len(row.Ciphertext) < aead.NonceSize()+aead.Overhead() {
		return nil, ErrDecrypt
	}
	nonce, body := row.Ciphertext[:aead.NonceSize()], row.Ciphertext[aead.NonceSize():]
	plain, err := aead.Open(nil, nonce, body, associatedData(row.ID, row.Provider, row.KeyID))
	if err != nil {
		return nil, ErrDecrypt
	}
	return plain, nil
}

func associatedData(id, providerID, keyID string) []byte {
	return []byte(id + "\x00" + providerID + "\x00" + keyID)
}
func validateSecret(secret string) error {
	if strings.TrimSpace(secret) == "" || len(secret) > MaxSecretBytes || !utf8.ValidString(secret) || providerHasControl(secret) {
		return ErrInvalidInput
	}
	return nil
}

func providerHasControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
func mask(secret string) string {
	return maskBytes([]byte(secret))
}
func maskBytes(secret []byte) string {
	end := len(secret)
	start, count := end, 0
	for start > 0 && count < 4 {
		_, width := utf8.DecodeLastRune(secret[:start])
		if width == 0 {
			break
		}
		start -= width
		count++
	}
	if start == 0 {
		return ""
	}
	return string(secret[start:end])
}
func toDTO(row storage.Credential) CredentialDTO {
	var caps []provider.Capability
	_ = json.Unmarshal([]byte(row.CapabilitiesJSON), &caps)
	dto := CredentialDTO{ID: row.ID, Provider: row.Provider, MaskedValue: "••••••" + row.MaskedSuffix, ValidationState: row.ValidationState, Capabilities: sortedCapabilities(caps), CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
	if !row.ValidatedAt.IsZero() {
		v := row.ValidatedAt
		dto.ValidatedAt = &v
	}
	return dto
}
func validCapabilities(registry *provider.Registry, providerID string, caps []provider.Capability) bool {
	seen := make(map[provider.Capability]bool)
	for _, c := range caps {
		if !registry.Supports(providerID, c) || seen[c] {
			return false
		}
		seen[c] = true
	}
	return true
}
func sortedCapabilities(caps []provider.Capability) []provider.Capability {
	out := append([]provider.Capability(nil), caps...)
	sortCapabilities(out)
	return out
}
func sortCapabilities(caps []provider.Capability) {
	sort.Slice(caps, func(i, j int) bool { return caps[i] < caps[j] })
}
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

type CapabilityReadiness struct {
	Ready  bool   `json:"ready"`
	Reason string `json:"reason,omitempty"`
}
type CredentialReadiness struct {
	Present      bool
	Valid        bool
	Provider     string
	Capabilities []provider.Capability
}

func Readiness(registry *provider.Registry, required map[provider.Capability]CredentialReadiness) map[provider.Capability]CapabilityReadiness {
	out := make(map[provider.Capability]CapabilityReadiness, len(required))
	for capability, state := range required {
		switch {
		case registry == nil || !registry.Supports(state.Provider, capability):
			out[capability] = CapabilityReadiness{Reason: "provider does not support this capability"}
		case !state.Present:
			out[capability] = CapabilityReadiness{Reason: "missing credential"}
		case !state.Valid:
			out[capability] = CapabilityReadiness{Reason: "credential is not validated"}
		case !hasCapability(state.Capabilities, capability):
			out[capability] = CapabilityReadiness{Reason: "credential is not verified for this capability"}
		default:
			out[capability] = CapabilityReadiness{Ready: true}
		}
	}
	return out
}

func hasCapability(capabilities []provider.Capability, want provider.Capability) bool {
	for _, capability := range capabilities {
		if capability == want {
			return true
		}
	}
	return false
}
