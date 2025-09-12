package storage

import (
	"context"
	"io"
	"time"
)

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

// Storage defines the interface for storage backends
type Storage interface {
	// Get retrieves an object from storage
	Get(ctx context.Context, key string) (io.ReadCloser, *ObjectInfo, error)

	// GetRange retrieves a byte range from an object
	GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, *ObjectInfo, error)

	// Put stores an object
	Put(ctx context.Context, key string, reader io.Reader, size int64, contentType string) (*ObjectInfo, error)

	// PutMultipart uploads a large object using multipart upload
	PutMultipart(ctx context.Context, key string, reader io.Reader, size int64, contentType string, partSize int64) (*ObjectInfo, error)

	// Delete removes an object
	Delete(ctx context.Context, key string) error

	// Exists checks if an object exists
	Exists(ctx context.Context, key string) (bool, error)

	// Stat retrieves object metadata without downloading content
	Stat(ctx context.Context, key string) (*ObjectInfo, error)

	// List returns a list of objects matching the options
	List(ctx context.Context, opts ListOptions) ([]*ObjectInfo, error)

	// GetPresignedURL generates a presigned URL for direct download
	GetPresignedURL(ctx context.Context, key string, expiry time.Duration) (string, error)

	// Close releases any resources held by the storage backend
	Close() error
}

// StorageType represents the type of storage backend
type StorageType string

const (
	StorageTypeLocal StorageType = "local"
	StorageTypeS3    StorageType = "s3"
)
