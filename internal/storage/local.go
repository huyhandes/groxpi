package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// LocalStorage stores objects as plain files under a base directory.
//
// Because its objects are real files it can hand out a path the transport can
// serve directly, so it implements ZeroCopyCapable. It has nothing to presign
// and deliberately does not implement Presignable.
type LocalStorage struct {
	baseDir string
}

var (
	_ Storage         = (*LocalStorage)(nil)
	_ ZeroCopyCapable = (*LocalStorage)(nil)
)

// localNotFound maps a filesystem error to the shared sentinel when it means
// "no such object", and wraps it as an ordinary failure otherwise.
func localNotFound(err error, key, op string) error {
	if os.IsNotExist(err) {
		return fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	return fmt.Errorf("failed to %s %s: %w", op, key, err)
}

// NewLocalStorage creates a new local filesystem storage backend
func NewLocalStorage(baseDir string) (*LocalStorage, error) {
	// Ensure base directory exists
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create base directory: %w", err)
	}

	return &LocalStorage{baseDir: baseDir}, nil
}

// buildPath constructs the full filesystem path
func (l *LocalStorage) buildPath(key string) string {
	return filepath.Join(l.baseDir, key)
}

// Get retrieves an object from local filesystem
func (l *LocalStorage) Get(ctx context.Context, key string) (io.ReadCloser, *ObjectInfo, error) {
	path := l.buildPath(key)

	file, err := os.Open(path)
	if err != nil {
		return nil, nil, localNotFound(err, key, "open")
	}

	stat, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, fmt.Errorf("failed to stat file: %w", err)
	}

	info := &ObjectInfo{
		Key:          key,
		Size:         stat.Size(),
		LastModified: stat.ModTime(),
	}

	return file, info, nil
}

// Put stores an object in local filesystem
func (l *LocalStorage) Put(ctx context.Context, key string, reader io.Reader, size int64, contentType string) (*ObjectInfo, error) {
	path := l.buildPath(key)

	// Ensure directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create directory: %w", err)
	}

	// Create temporary file first
	tmpFile, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	// Ensure cleanup on error
	defer func() {
		if tmpFile != nil {
			_ = tmpFile.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	// Copy data
	written, err := io.Copy(tmpFile, reader)
	if err != nil {
		return nil, fmt.Errorf("failed to write file: %w", err)
	}

	// Close temp file
	if err := tmpFile.Close(); err != nil {
		return nil, fmt.Errorf("failed to close temp file: %w", err)
	}
	tmpFile = nil // Prevent defer cleanup

	// Move to final location
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return nil, fmt.Errorf("failed to move file: %w", err)
	}

	return &ObjectInfo{
		Key:         key,
		Size:        written,
		ContentType: contentType,
	}, nil
}

// Delete removes an object from local filesystem
func (l *LocalStorage) Delete(ctx context.Context, key string) error {
	path := l.buildPath(key)

	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete file: %w", err)
	}

	return nil
}

// Exists checks if an object exists in local filesystem
func (l *LocalStorage) Exists(ctx context.Context, key string) (bool, error) {
	path := l.buildPath(key)

	_, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to stat file: %w", err)
	}

	return true, nil
}

// Stat retrieves object metadata without opening the file
func (l *LocalStorage) Stat(ctx context.Context, key string) (*ObjectInfo, error) {
	path := l.buildPath(key)

	stat, err := os.Stat(path)
	if err != nil {
		return nil, localNotFound(err, key, "stat")
	}

	return &ObjectInfo{
		Key:          key,
		Size:         stat.Size(),
		LastModified: stat.ModTime(),
	}, nil
}

// List returns a list of objects matching the options
func (l *LocalStorage) List(ctx context.Context, opts ListOptions) ([]*ObjectInfo, error) {
	pattern := filepath.Join(l.baseDir, opts.Prefix+"*")

	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("failed to list files: %w", err)
	}

	var objects []*ObjectInfo
	count := 0

	for _, path := range matches {
		if opts.MaxKeys > 0 && count >= opts.MaxKeys {
			break
		}

		stat, err := os.Stat(path)
		if err != nil {
			continue // Skip files we can't stat
		}

		if stat.IsDir() {
			continue // Skip directories
		}

		key, err := filepath.Rel(l.baseDir, path)
		if err != nil {
			continue
		}

		// Skip if before StartAfter
		if opts.StartAfter != "" && key <= opts.StartAfter {
			continue
		}

		objects = append(objects, &ObjectInfo{
			Key:          key,
			Size:         stat.Size(),
			LastModified: stat.ModTime(),
		})
		count++
	}

	return objects, nil
}

// Close releases any resources (no-op for local storage)
func (l *LocalStorage) Close() error {
	return nil
}

// GetFilePath returns the local file path for zero-copy serving. The transport
// serves this path with net/http rather than opening the object and copying the
// body itself, so this is the hot read path.
func (l *LocalStorage) GetFilePath(ctx context.Context, key string) (string, error) {
	path := l.buildPath(key)

	// Check if file exists
	if _, err := os.Stat(path); err != nil {
		return "", localNotFound(err, key, "stat")
	}

	return path, nil
}
