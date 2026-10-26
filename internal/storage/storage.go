package storage

import (
	"errors"
	"time"
)

// ErrNotFound is the sentinel every backend returns when a key is absent.
// Adapters wrap it with backend detail; callers must test with errors.Is so
// that a genuine backend failure is never mistaken for a cache miss.
var ErrNotFound = errors.New("object not found")

// ErrStorage wraps a storage failure on a file the store holds: the miss is
// not the file's absence, and redirecting upstream would not be the answer.
var ErrStorage = errors.New("storage operation failed")

// ObjectInfo contains metadata about a stored object
type ObjectInfo struct {
	Size         int64
	LastModified time.Time
}
