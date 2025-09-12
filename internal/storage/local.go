package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// LocalStorage implements Storage interface for local filesystem
type LocalStorage struct {
	baseDir string
}

// NewLocalStorage creates a new local filesystem storage backend
func NewLocalStorage(baseDir string) (*LocalStorage, error) {
	// Ensure base directory exists
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create base directory: %w", err)
	}

	return &LocalStorage{
		baseDir: baseDir,
	}, nil
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
		if os.IsNotExist(err) {
			return nil, nil, fmt.Errorf("object not found: %s", key)
		}
		return nil, nil, fmt.Errorf("failed to open file: %w", err)
	}

	stat, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, fmt.Errorf("failed to stat file: %w", err)
	}

	info := &ObjectInfo{
		Key:          key,
		Size:         stat.Size(),
		LastModified: stat.ModTime(),
	}

	return file, info, nil
}

// GetRange retrieves a byte range from a file
func (l *LocalStorage) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, *ObjectInfo, error) {
	path := l.buildPath(key)

	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, fmt.Errorf("object not found: %s", key)
		}
		return nil, nil, fmt.Errorf("failed to open file: %w", err)
	}

	stat, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, fmt.Errorf("failed to stat file: %w", err)
	}

	// Seek to offset if specified
	if offset > 0 {
		_, err = file.Seek(offset, io.SeekStart)
		if err != nil {
			file.Close()
			return nil, nil, fmt.Errorf("failed to seek: %w", err)
		}
	}

	// Wrap in a limited reader if length is specified
	var reader io.ReadCloser = file
	if length > 0 {
		reader = &limitedReadCloser{
			Reader: io.LimitReader(file, length),
			Closer: file,
		}
	}

	info := &ObjectInfo{
		Key:          key,
		Size:         stat.Size(),
		LastModified: stat.ModTime(),
	}

	return reader, info, nil
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
			tmpFile.Close()
			os.Remove(tmpPath)
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
		os.Remove(tmpPath)
		return nil, fmt.Errorf("failed to move file: %w", err)
	}

	return &ObjectInfo{
		Key:         key,
		Size:        written,
		ContentType: contentType,
	}, nil
}

// PutMultipart is the same as Put for local storage
func (l *LocalStorage) PutMultipart(ctx context.Context, key string, reader io.Reader, size int64, contentType string, partSize int64) (*ObjectInfo, error) {
	return l.Put(ctx, key, reader, size, contentType)
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
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("object not found: %s", key)
		}
		return nil, fmt.Errorf("failed to stat file: %w", err)
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

// GetPresignedURL is not supported for local storage
func (l *LocalStorage) GetPresignedURL(ctx context.Context, key string, expiry time.Duration) (string, error) {
	// For local storage, return a file:// URL
	path := l.buildPath(key)
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("failed to get absolute path: %w", err)
	}
	return "file://" + absPath, nil
}

// Close releases any resources (no-op for local storage)
func (l *LocalStorage) Close() error {
	return nil
}

// limitedReadCloser wraps a limited reader with a closer
type limitedReadCloser struct {
	io.Reader
	io.Closer
}
