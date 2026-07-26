package storage

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrNotFound is the sentinel every backend returns when a key is absent.
// Adapters wrap it with backend detail; callers must test with errors.Is so
// that a genuine backend failure is never mistaken for a cache miss.
var ErrNotFound = errors.New("object not found")

// ObjectInfo contains metadata about a stored object
type ObjectInfo struct {
	Key          string
	Size         int64
	LastModified time.Time
	ETag         string
	ContentType  string
	Metadata     map[string]string
}

// ListOptions configures object listing
type ListOptions struct {
	Prefix     string
	MaxKeys    int
	StartAfter string
}

// Storage is the core contract every backend satisfies. It is deliberately
// small: anything a backend can only fake belongs in a capability interface
// below, so callers never have to ask which backend they are talking to.
type Storage interface {
	// Get opens an object for reading. The returned ObjectInfo is complete
	// before any byte of the body is produced, so a caller can emit response
	// headers and only then copy from the reader. Returns an error matching
	// ErrNotFound if the key does not exist.
	Get(ctx context.Context, key string) (io.ReadCloser, *ObjectInfo, error)

	// Put stores an object.
	Put(ctx context.Context, key string, reader io.Reader, size int64, contentType string) (*ObjectInfo, error)

	// Stat retrieves object metadata without downloading content. Returns an
	// error matching ErrNotFound if the key does not exist.
	Stat(ctx context.Context, key string) (*ObjectInfo, error)

	// Delete removes an object. Deleting a missing object is not an error.
	Delete(ctx context.Context, key string) error

	// Exists reports whether an object is present. A missing object is
	// (false, nil); a non-nil error always means the check itself failed.
	Exists(ctx context.Context, key string) (bool, error)

	// List returns the objects matching opts.
	List(ctx context.Context, opts ListOptions) ([]*ObjectInfo, error)

	// Close releases any resources held by the backend.
	Close() error
}

// ZeroCopyCapable is implemented only by backends whose objects are real files
// on the local filesystem, so the transport can hand the path to the kernel
// (sendfile) instead of copying bytes through user space.
//
// Callers opt in with a type assertion:
//
//	if zc, ok := s.(storage.ZeroCopyCapable); ok { ... }
type ZeroCopyCapable interface {
	// GetFilePath returns the local filesystem path backing key. Returns an
	// error matching ErrNotFound if the key does not exist.
	GetFilePath(ctx context.Context, key string) (string, error)
}

// Presignable is implemented only by backends that can mint a time-limited URL
// a client can fetch directly, bypassing this proxy.
type Presignable interface {
	// GetPresignedURL returns a URL valid for expiry.
	GetPresignedURL(ctx context.Context, key string, expiry time.Duration) (string, error)
}

// StorageType represents the type of storage backend
type StorageType string

const (
	StorageTypeLocal StorageType = "local"
	StorageTypeS3    StorageType = "s3"
)
