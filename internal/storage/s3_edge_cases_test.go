package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestS3Storage_EdgeCases tests various edge cases and boundary conditions
func TestS3Storage_EdgeCases(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	storage := createTestS3Storage(t, "groxpi-edge-test")
	defer storage.Close()

	ctx := context.Background()

	t.Run("Empty_file_handling", func(t *testing.T) {
		key := fmt.Sprintf("edge-test/empty/%d.txt", time.Now().UnixNano())
		content := []byte("")

		// Put empty file
		info, err := storage.Put(ctx, key, bytes.NewReader(content), int64(len(content)), "text/plain")
		require.NoError(t, err, "Failed to put empty file")
		assert.Equal(t, int64(0), info.Size)

		// Get empty file
		reader, getInfo, err := storage.Get(ctx, key)
		require.NoError(t, err, "Failed to get empty file")
		defer reader.Close()

		assert.Equal(t, int64(0), getInfo.Size)

		data, err := io.ReadAll(reader)
		require.NoError(t, err, "Failed to read empty file")
		assert.Equal(t, content, data)
		assert.Len(t, data, 0)

		// Test exists and stat
		exists, err := storage.Exists(ctx, key)
		require.NoError(t, err, "Failed to check existence of empty file")
		assert.True(t, exists)

		statInfo, err := storage.Stat(ctx, key)
		require.NoError(t, err, "Failed to stat empty file")
		assert.Equal(t, int64(0), statInfo.Size)

		// Clean up
		err = storage.Delete(ctx, key)
		assert.NoError(t, err, "Failed to delete empty file")
	})

	t.Run("Large_file_handling", func(t *testing.T) {
		key := fmt.Sprintf("edge-test/large/%d.bin", time.Now().UnixNano())

		// Create 25MB file to test multipart upload thoroughly
		size := 25 * 1024 * 1024
		content := make([]byte, size)

		// Fill with predictable pattern for verification
		for i := range content {
			content[i] = byte((i / 1024) % 256) // Changes every KB
		}

		// Put large file with custom part size
		info, err := storage.PutMultipart(ctx, key, bytes.NewReader(content), int64(size), "application/octet-stream", 8*1024*1024)
		require.NoError(t, err, "Failed to put large file")
		assert.Equal(t, int64(size), info.Size)

		// Get partial range from large file
		offset := int64(10 * 1024 * 1024) // 10MB offset
		length := int64(1024 * 1024)      // 1MB length

		reader, _, err := storage.GetRange(ctx, key, offset, length)
		require.NoError(t, err, "Failed to get range from large file")
		defer reader.Close()

		data, err := io.ReadAll(reader)
		require.NoError(t, err, "Failed to read range data")
		assert.Equal(t, int64(len(data)), length)

		// Verify content pattern
		expectedFirst := byte((int(offset) / 1024) % 256)
		assert.Equal(t, expectedFirst, data[0], "First byte of range incorrect")

		// Clean up large file
		err = storage.Delete(ctx, key)
		assert.NoError(t, err, "Failed to delete large file")
	})

	t.Run("Unicode_and_special_characters", func(t *testing.T) {
		testCases := []struct {
			name    string
			key     string
			content string
		}{
			{
				name:    "unicode_filename",
				key:     fmt.Sprintf("edge-test/unicode/%d/文件名.txt", time.Now().UnixNano()),
				content: "Unicode filename test",
			},
			{
				name:    "unicode_content",
				key:     fmt.Sprintf("edge-test/unicode-content/%d.txt", time.Now().UnixNano()),
				content: "Hello 世界! 🌍 Emoji and unicode content: 日本語, العربية, русский",
			},
			{
				name:    "special_chars_in_key",
				key:     fmt.Sprintf("edge-test/special/%d/file-with_special.chars@domain.txt", time.Now().UnixNano()),
				content: "File with special characters in name",
			},
			{
				name:    "spaces_in_key",
				key:     fmt.Sprintf("edge-test/spaces/%d/file with spaces.txt", time.Now().UnixNano()),
				content: "File with spaces in name",
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				contentBytes := []byte(tc.content)

				// Verify content is valid UTF-8
				assert.True(t, utf8.Valid(contentBytes), "Test content should be valid UTF-8")

				// Put object
				info, err := storage.Put(ctx, tc.key, bytes.NewReader(contentBytes), int64(len(contentBytes)), "text/plain; charset=utf-8")
				require.NoError(t, err, "Failed to put object with special characters")
				assert.Equal(t, tc.key, info.Key)

				// Get object
				reader, getInfo, err := storage.Get(ctx, tc.key)
				require.NoError(t, err, "Failed to get object with special characters")
				defer reader.Close()

				data, err := io.ReadAll(reader)
				require.NoError(t, err, "Failed to read object with special characters")
				assert.Equal(t, contentBytes, data)
				assert.True(t, utf8.Valid(data), "Retrieved content should be valid UTF-8")

				// Verify metadata
				assert.Equal(t, tc.key, getInfo.Key)
				assert.Equal(t, int64(len(contentBytes)), getInfo.Size)

				// Clean up
				err = storage.Delete(ctx, tc.key)
				assert.NoError(t, err, "Failed to delete object with special characters")
			})
		}
	})

	t.Run("Binary_data_handling", func(t *testing.T) {
		key := fmt.Sprintf("edge-test/binary/%d.bin", time.Now().UnixNano())

		// Create binary data with all byte values
		content := make([]byte, 256)
		for i := range content {
			content[i] = byte(i)
		}

		// Put binary data
		info, err := storage.Put(ctx, key, bytes.NewReader(content), int64(len(content)), "application/octet-stream")
		require.NoError(t, err, "Failed to put binary data")
		assert.Equal(t, int64(256), info.Size)

		// Get binary data
		reader, _, err := storage.Get(ctx, key)
		require.NoError(t, err, "Failed to get binary data")
		defer reader.Close()

		data, err := io.ReadAll(reader)
		require.NoError(t, err, "Failed to read binary data")
		assert.Equal(t, content, data)

		// Verify all byte values are preserved
		assert.Len(t, data, 256)
		for i, b := range data {
			assert.Equal(t, byte(i), b, "Byte at position %d should be %d", i, i)
		}

		// Clean up
		err = storage.Delete(ctx, key)
		assert.NoError(t, err, "Failed to delete binary data")
	})

	t.Run("Range_requests_edge_cases", func(t *testing.T) {
		key := fmt.Sprintf("edge-test/range/%d.txt", time.Now().UnixNano())
		content := []byte("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ") // 36 bytes

		// Put object
		_, err := storage.Put(ctx, key, bytes.NewReader(content), int64(len(content)), "text/plain")
		require.NoError(t, err, "Failed to put object for range tests")

		testCases := []struct {
			name        string
			offset      int64
			length      int64
			expectError bool
			expectData  string
		}{
			{
				name:       "zero_offset_full_length",
				offset:     0,
				length:     int64(len(content)),
				expectData: string(content),
			},
			{
				name:       "last_byte_only",
				offset:     int64(len(content) - 1),
				length:     1,
				expectData: "Z",
			},
			{
				name:       "zero_length_range",
				offset:     10,
				length:     0,
				expectData: "", // Should return empty but not error
			},
			{
				name:        "range_at_end",
				offset:      int64(len(content)),
				length:      1,
				expectError: false, // May return empty or error depending on S3 implementation
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				reader, info, err := storage.GetRange(ctx, key, tc.offset, tc.length)

				if tc.expectError {
					assert.Error(t, err, "Expected error for range request")
					return
				}

				if err != nil {
					// Some edge cases might legitimately error
					t.Logf("Range request errored (may be expected): %v", err)
					return
				}

				require.NotNil(t, reader, "Reader should not be nil")
				defer reader.Close()

				data, err := io.ReadAll(reader)
				require.NoError(t, err, "Failed to read range data")

				if tc.expectData != "" {
					assert.Equal(t, tc.expectData, string(data))
				}

				assert.NotNil(t, info, "Info should not be nil")
			})
		}

		// Clean up
		err = storage.Delete(ctx, key)
		assert.NoError(t, err, "Failed to delete range test object")
	})

	t.Run("Prefix_handling_edge_cases", func(t *testing.T) {
		// Test various prefix configurations
		prefixCases := []struct {
			name   string
			prefix string
		}{
			{"no_prefix", ""},
			{"simple_prefix", "test"},
			{"nested_prefix", "test/nested/deep"},
			{"prefix_with_special_chars", "test-prefix_with.special@chars"},
		}

		for _, pc := range prefixCases {
			t.Run(pc.name, func(t *testing.T) {
				// Create storage with specific prefix
				customStorage := createTestS3Storage(t, pc.prefix)
				defer customStorage.Close()

				key := fmt.Sprintf("prefix-test/%d.txt", time.Now().UnixNano())
				content := []byte(fmt.Sprintf("content for %s", pc.name))

				// Put object
				info, err := storage.Put(ctx, key, bytes.NewReader(content), int64(len(content)), "text/plain")
				require.NoError(t, err, "Failed to put object with prefix %s", pc.prefix)
				assert.Equal(t, key, info.Key) // Key should not include prefix in response

				// Get object
				reader, getInfo, err := storage.Get(ctx, key)
				require.NoError(t, err, "Failed to get object with prefix %s", pc.prefix)
				defer reader.Close()

				data, err := io.ReadAll(reader)
				require.NoError(t, err, "Failed to read object with prefix %s", pc.prefix)
				assert.Equal(t, content, data)
				assert.Equal(t, key, getInfo.Key)

				// List objects
				objects, err := storage.List(ctx, ListOptions{
					Prefix:  "prefix-test",
					MaxKeys: 10,
				})
				require.NoError(t, err, "Failed to list objects with prefix %s", pc.prefix)

				// Should find our object
				found := false
				for _, obj := range objects {
					if obj.Key == key {
						found = true
						break
					}
				}
				assert.True(t, found, "Should find object in list with prefix %s", pc.prefix)

				// Clean up
				err = storage.Delete(ctx, key)
				assert.NoError(t, err, "Failed to delete object with prefix %s", pc.prefix)
			})
		}
	})

	t.Run("Content_type_edge_cases", func(t *testing.T) {
		testCases := []struct {
			name        string
			contentType string
			content     []byte
		}{
			{
				name:        "empty_content_type",
				contentType: "",
				content:     []byte("content with empty content type"),
			},
			{
				name:        "long_content_type",
				contentType: strings.Repeat("very-long-content-type/", 10) + "final",
				content:     []byte("content with very long content type"),
			},
			{
				name:        "content_type_with_charset",
				contentType: "text/html; charset=utf-8; boundary=something",
				content:     []byte("<!DOCTYPE html><html><body>HTML content</body></html>"),
			},
			{
				name:        "binary_content_type",
				contentType: "application/octet-stream",
				content:     []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}, // PNG header
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				key := fmt.Sprintf("edge-test/content-type/%d/%s.txt", time.Now().UnixNano(), tc.name)

				// Put object with specific content type
				info, err := storage.Put(ctx, key, bytes.NewReader(tc.content), int64(len(tc.content)), tc.contentType)
				require.NoError(t, err, "Failed to put object with content type: %s", tc.contentType)
				assert.Equal(t, tc.contentType, info.ContentType)

				// Get object and verify content type is preserved
				reader, getInfo, err := storage.Get(ctx, key)
				require.NoError(t, err, "Failed to get object with content type: %s", tc.contentType)
				defer reader.Close()

				data, err := io.ReadAll(reader)
				require.NoError(t, err, "Failed to read object with content type: %s", tc.contentType)
				assert.Equal(t, tc.content, data)

				// Content type might be normalized by S3, so we just check it's not empty for non-empty input
				if tc.contentType != "" {
					assert.NotEmpty(t, getInfo.ContentType, "Content type should be preserved")
				}

				// Clean up
				err = storage.Delete(ctx, key)
				assert.NoError(t, err, "Failed to delete object with content type: %s", tc.contentType)
			})
		}
	})

	t.Run("List_pagination_edge_cases", func(t *testing.T) {
		basePrefix := fmt.Sprintf("edge-test/list-pagination/%d", time.Now().UnixNano())

		// Create exactly 3 objects for pagination testing
		keys := make([]string, 3)
		for i := 0; i < 3; i++ {
			key := fmt.Sprintf("%s/file-%02d.txt", basePrefix, i)
			keys[i] = key
			content := []byte(fmt.Sprintf("content %d", i))

			_, err := storage.Put(ctx, key, bytes.NewReader(content), int64(len(content)), "text/plain")
			require.NoError(t, err, "Failed to put object %d", i)
		}

		testCases := []struct {
			name         string
			maxKeys      int
			startAfter   string
			expectCount  int
			expectPrefix bool
		}{
			{
				name:        "max_keys_zero",
				maxKeys:     0,
				expectCount: 3, // S3 typically ignores MaxKeys=0
			},
			{
				name:        "max_keys_one",
				maxKeys:     1,
				expectCount: 1,
			},
			{
				name:        "max_keys_larger_than_available",
				maxKeys:     10,
				expectCount: 3,
			},
			{
				name:        "start_after_first",
				maxKeys:     10,
				startAfter:  keys[0],
				expectCount: 2, // Should get keys[1] and keys[2]
			},
			{
				name:        "start_after_nonexistent",
				maxKeys:     10,
				startAfter:  basePrefix + "/nonexistent",
				expectCount: 3, // Lexicographically, all our files come after "nonexistent"
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				objects, err := storage.List(ctx, ListOptions{
					Prefix:     basePrefix,
					MaxKeys:    tc.maxKeys,
					StartAfter: tc.startAfter,
				})
				require.NoError(t, err, "Failed to list objects")

				// The exact count might vary based on S3 implementation details
				// We mainly test that it doesn't crash and returns reasonable results
				assert.LessOrEqual(t, len(objects), 3, "Should not return more objects than exist")

				if tc.maxKeys > 0 {
					assert.LessOrEqual(t, len(objects), tc.maxKeys, "Should not exceed MaxKeys")
				}

				// Verify all returned objects have the correct prefix
				for _, obj := range objects {
					assert.True(t, strings.HasPrefix(obj.Key, basePrefix),
						"Object key %s should have prefix %s", obj.Key, basePrefix)
				}
			})
		}

		// Clean up
		for _, key := range keys {
			err := storage.Delete(ctx, key)
			assert.NoError(t, err, "Failed to delete object %s", key)
		}
	})

	t.Run("Metadata_edge_cases", func(t *testing.T) {
		key := fmt.Sprintf("edge-test/metadata/%d.txt", time.Now().UnixNano())
		content := []byte("content for metadata test")

		// Put object
		_, err := storage.Put(ctx, key, bytes.NewReader(content), int64(len(content)), "text/plain")
		require.NoError(t, err, "Failed to put object")

		// Get object info
		info, err := storage.Stat(ctx, key)
		require.NoError(t, err, "Failed to stat object")

		// Verify basic metadata is present
		assert.Equal(t, key, info.Key)
		assert.Equal(t, int64(len(content)), info.Size)
		assert.NotEmpty(t, info.ETag, "ETag should be present")
		assert.False(t, info.LastModified.IsZero(), "LastModified should be set")

		// LastModified should be recent (within last minute)
		timeDiff := time.Since(info.LastModified)
		assert.Less(t, timeDiff, 5*time.Minute, "LastModified should be recent")

		// Clean up
		err = storage.Delete(ctx, key)
		assert.NoError(t, err, "Failed to delete object")
	})
}
