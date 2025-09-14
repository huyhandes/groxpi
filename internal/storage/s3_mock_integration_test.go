package storage

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"
	"time"
)

// TestS3Storage runs integration tests for S3 storage
// Requires MinIO or S3-compatible storage to be running
func TestS3Storage(t *testing.T) {
	// Skip if S3 credentials are not set
	if os.Getenv("TEST_S3_ENDPOINT") == "" {
		t.Skip("S3 integration tests require TEST_S3_ENDPOINT to be set")
	}

	cfg := &S3Config{
		Endpoint:        os.Getenv("TEST_S3_ENDPOINT"),
		AccessKeyID:     os.Getenv("TEST_S3_ACCESS_KEY"),
		SecretAccessKey: os.Getenv("TEST_S3_SECRET_KEY"),
		Bucket:          os.Getenv("TEST_S3_BUCKET"),
		Region:          "us-east-1",
		UseSSL:          false,
		ForcePathStyle:  true, // MinIO requires path-style
		Prefix:          "test",
	}

	storage, err := NewS3Storage(cfg)
	if err != nil {
		t.Fatalf("Failed to create S3 storage: %v", err)
	}
	defer func() { _ = storage.Close() }()

	ctx := context.Background()

	t.Run("PutAndGet", func(t *testing.T) {
		key := "test/file.txt"
		content := []byte("Hello, S3!")

		// Put object
		info, err := storage.Put(ctx, key, bytes.NewReader(content), int64(len(content)), "text/plain")
		if err != nil {
			t.Fatalf("Failed to put object: %v", err)
		}

		if info.Size != int64(len(content)) {
			t.Errorf("Expected size %d, got %d", len(content), info.Size)
		}

		// Get object
		reader, _, err := storage.Get(ctx, key)
		if err != nil {
			t.Fatalf("Failed to get object: %v", err)
		}
		defer func() { _ = reader.Close() }()

		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("Failed to read object: %v", err)
		}

		if !bytes.Equal(data, content) {
			t.Errorf("Content mismatch: expected %s, got %s", content, data)
		}

		// Clean up
		_ = storage.Delete(ctx, key)
	})

	t.Run("GetRange", func(t *testing.T) {
		key := "test/range-file.txt"
		content := []byte("0123456789ABCDEF")

		// Put object
		_, err := storage.Put(ctx, key, bytes.NewReader(content), int64(len(content)), "text/plain")
		if err != nil {
			t.Fatalf("Failed to put object: %v", err)
		}

		// Get range
		reader, _, err := storage.GetRange(ctx, key, 5, 5)
		if err != nil {
			t.Fatalf("Failed to get range: %v", err)
		}
		defer func() { _ = reader.Close() }()

		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("Failed to read range: %v", err)
		}

		expected := content[5:10]
		if !bytes.Equal(data, expected) {
			t.Errorf("Range mismatch: expected %s, got %s", expected, data)
		}

		// Clean up
		_ = storage.Delete(ctx, key)
	})

	t.Run("Exists", func(t *testing.T) {
		key := "test/exists.txt"
		content := []byte("test")

		// Check non-existent
		exists, err := storage.Exists(ctx, key)
		if err != nil {
			t.Fatalf("Failed to check existence: %v", err)
		}
		if exists {
			t.Error("Object should not exist")
		}

		// Put object
		_, err = storage.Put(ctx, key, bytes.NewReader(content), int64(len(content)), "text/plain")
		if err != nil {
			t.Fatalf("Failed to put object: %v", err)
		}

		// Check existent
		exists, err = storage.Exists(ctx, key)
		if err != nil {
			t.Fatalf("Failed to check existence: %v", err)
		}
		if !exists {
			t.Error("Object should exist")
		}

		// Clean up
		_ = storage.Delete(ctx, key)
	})

	t.Run("List", func(t *testing.T) {
		// Put multiple objects
		keys := []string{"test/list1.txt", "test/list2.txt", "test/list3.txt"}
		for _, key := range keys {
			content := []byte(key)
			_, err := storage.Put(ctx, key, bytes.NewReader(content), int64(len(content)), "text/plain")
			if err != nil {
				t.Fatalf("Failed to put object %s: %v", key, err)
			}
		}

		// List objects
		objects, err := storage.List(ctx, ListOptions{
			Prefix:  "test/list",
			MaxKeys: 10,
		})
		if err != nil {
			t.Fatalf("Failed to list objects: %v", err)
		}

		if len(objects) != 3 {
			t.Errorf("Expected 3 objects, got %d", len(objects))
		}

		// Clean up
		for _, key := range keys {
			_ = storage.Delete(ctx, key)
		}
	})

	t.Run("PresignedURL", func(t *testing.T) {
		key := "test/presigned.txt"
		content := []byte("presigned content")

		// Put object
		_, err := storage.Put(ctx, key, bytes.NewReader(content), int64(len(content)), "text/plain")
		if err != nil {
			t.Fatalf("Failed to put object: %v", err)
		}

		// Generate presigned URL
		url, err := storage.GetPresignedURL(ctx, key, 1*time.Hour)
		if err != nil {
			t.Fatalf("Failed to generate presigned URL: %v", err)
		}

		if url == "" {
			t.Error("Presigned URL should not be empty")
		}

		// Clean up
		_ = storage.Delete(ctx, key)
	})

	t.Run("Multipart", func(t *testing.T) {
		key := "test/multipart.bin"
		// Create 15MB of data to trigger multipart
		size := 15 * 1024 * 1024
		content := make([]byte, size)
		for i := range content {
			content[i] = byte(i % 256)
		}

		// Put with multipart
		info, err := storage.PutMultipart(ctx, key, bytes.NewReader(content), int64(size), "application/octet-stream", 5*1024*1024)
		if err != nil {
			t.Fatalf("Failed to put multipart object: %v", err)
		}

		if info.Size != int64(size) {
			t.Errorf("Expected size %d, got %d", size, info.Size)
		}

		// Verify we can read it back
		reader, _, err := storage.Get(ctx, key)
		if err != nil {
			t.Fatalf("Failed to get multipart object: %v", err)
		}
		defer func() { _ = reader.Close() }()

		// Read first 1KB to verify
		buffer := make([]byte, 1024)
		n, err := io.ReadFull(reader, buffer)
		if err != nil {
			t.Fatalf("Failed to read multipart object: %v", err)
		}
		if n != 1024 {
			t.Errorf("Expected to read 1024 bytes, got %d", n)
		}

		// Clean up
		_ = storage.Delete(ctx, key)
	})
}

// TestLocalStorage runs unit tests for local storage
func TestLocalStorage(t *testing.T) {
	tmpDir := t.TempDir()

	storage, err := NewLocalStorage(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create local storage: %v", err)
	}
	defer func() { _ = storage.Close() }()

	ctx := context.Background()

	t.Run("PutAndGet", func(t *testing.T) {
		key := "test/file.txt"
		content := []byte("Hello, Local!")

		// Put object
		info, err := storage.Put(ctx, key, bytes.NewReader(content), int64(len(content)), "text/plain")
		if err != nil {
			t.Fatalf("Failed to put object: %v", err)
		}

		if info.Size != int64(len(content)) {
			t.Errorf("Expected size %d, got %d", len(content), info.Size)
		}

		// Get object
		reader, _, err := storage.Get(ctx, key)
		if err != nil {
			t.Fatalf("Failed to get object: %v", err)
		}
		defer func() { _ = reader.Close() }()

		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("Failed to read object: %v", err)
		}

		if !bytes.Equal(data, content) {
			t.Errorf("Content mismatch: expected %s, got %s", content, data)
		}
	})

	t.Run("GetRange", func(t *testing.T) {
		key := "test/range-file.txt"
		content := []byte("0123456789ABCDEF")

		// Put object
		_, err := storage.Put(ctx, key, bytes.NewReader(content), int64(len(content)), "text/plain")
		if err != nil {
			t.Fatalf("Failed to put object: %v", err)
		}

		// Get range
		reader, _, err := storage.GetRange(ctx, key, 5, 5)
		if err != nil {
			t.Fatalf("Failed to get range: %v", err)
		}
		defer func() { _ = reader.Close() }()

		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("Failed to read range: %v", err)
		}

		expected := content[5:10]
		if !bytes.Equal(data, expected) {
			t.Errorf("Range mismatch: expected %s, got %s", expected, data)
		}
	})

	t.Run("Delete", func(t *testing.T) {
		key := "test/delete.txt"
		content := []byte("delete me")

		// Put object
		_, err := storage.Put(ctx, key, bytes.NewReader(content), int64(len(content)), "text/plain")
		if err != nil {
			t.Fatalf("Failed to put object: %v", err)
		}

		// Delete object
		err = storage.Delete(ctx, key)
		if err != nil {
			t.Fatalf("Failed to delete object: %v", err)
		}

		// Verify deleted
		exists, err := storage.Exists(ctx, key)
		if err != nil {
			t.Fatalf("Failed to check existence: %v", err)
		}
		if exists {
			t.Error("Object should not exist after deletion")
		}
	})
}
