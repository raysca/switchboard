package retention

import (
	"errors"
	"fmt"
	"time"
)

// Policy defines data retention durations and limits.
type Policy struct {
	MetadataRetention time.Duration
	ContentRetention  time.Duration
	BatchSize         int
}

// Validate ensures retention periods and batch limits are valid.
func (p Policy) Validate() error {
	if p.MetadataRetention < 0 {
		return errors.New("metadata retention must be non-negative")
	}
	if p.ContentRetention < 0 {
		return errors.New("content retention must be non-negative")
	}
	if p.MetadataRetention > 0 && p.ContentRetention > p.MetadataRetention {
		return fmt.Errorf("content retention (%s) cannot exceed metadata retention (%s)", p.ContentRetention, p.MetadataRetention)
	}
	if p.BatchSize < 0 {
		return errors.New("batch size must be non-negative")
	}
	return nil
}
