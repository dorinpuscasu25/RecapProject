// Package ids generates time-ordered UUIDv7 identifiers used across all entities.
package ids

import (
	"github.com/google/uuid"
)

// New returns a UUIDv7 (time-ordered, index friendly).
func New() uuid.UUID {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.New()
	}
	return id
}

// Parse parses a UUID string and reports whether it was valid.
func Parse(s string) (uuid.UUID, bool) {
	id, err := uuid.Parse(s)
	return id, err == nil
}
