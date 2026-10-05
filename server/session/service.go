// Package session provides session query and lifecycle management domain services.
package session

import (
	"context"

	"govoice/server/storage"
)

// Repository defines the persistence operations needed by session.Service.
type Repository interface {
	Create(ctx context.Context, v storage.SessionRecord) error
	Get(ctx context.Context, id string) (storage.SessionRecord, error)
	List(ctx context.Context, filter storage.SessionFilter) ([]storage.SessionRecord, error)
	End(ctx context.Context, id string) error
	AddTurn(ctx context.Context, v storage.TurnRecord) error
	Turns(ctx context.Context, sessionID string) ([]storage.TurnRecord, error)
}

// Service manages product sessions.
type Service struct {
	repo Repository
}

// NewService constructs a session service.
func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

// Create persists a new session record.
func (s *Service) Create(ctx context.Context, v storage.SessionRecord) error {
	return s.repo.Create(ctx, v)
}

// Get retrieves a session by ID.
func (s *Service) Get(ctx context.Context, id string) (storage.SessionRecord, error) {
	return s.repo.Get(ctx, id)
}

// List returns sessions matching filter.
func (s *Service) List(ctx context.Context, filter storage.SessionFilter) ([]storage.SessionRecord, error) {
	return s.repo.List(ctx, filter)
}

// End transitions a session to ended state.
func (s *Service) End(ctx context.Context, id string) error {
	return s.repo.End(ctx, id)
}

// AddTurn appends a turn to a session.
func (s *Service) AddTurn(ctx context.Context, v storage.TurnRecord) error {
	return s.repo.AddTurn(ctx, v)
}

// Turns returns all turns for a session ordered by ordinal.
func (s *Service) Turns(ctx context.Context, sessionID string) ([]storage.TurnRecord, error) {
	return s.repo.Turns(ctx, sessionID)
}
