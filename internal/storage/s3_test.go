package storage

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestS3Storage_CalculateOptimalPartSize tests the part size calculation logic
func TestS3Storage_CalculateOptimalPartSize(t *testing.T) {
	// Create S3 storage instance for testing
	cfg := &S3Config{
		Endpoint: "test.endpoint",
		Bucket:   "test-bucket",
		PartSize: 10 * 1024 * 1024, // 10MB default
	}
	storage, err := NewS3Storage(cfg)
	if err != nil {
		// Skip if we can't create storage (e.g., connection issues)
		t.Skipf("Cannot create S3 storage for testing: %v", err)
	}
	defer func() { _ = storage.Close() }()

	tests := []struct {
		name        string
		fileSize    int64
		expectedMin int64
		expectedMax int64
		description string
	}{
		{
			name:        "small_file_10MB",
			fileSize:    10 * 1024 * 1024, // 10MB
			expectedMin: 5 * 1024 * 1024,  // 5MB minimum
			expectedMax: 32 * 1024 * 1024, // Should use default or small part size
			description: "Small files should use minimum viable part size",
		},
		{
			name:        "medium_file_50MB",
			fileSize:    50 * 1024 * 1024, // 50MB
			expectedMin: 5 * 1024 * 1024,  // 5MB minimum
			expectedMax: 32 * 1024 * 1024, // Should use default part size
			description: "Medium files should use default part size",
		},
		{
			name:        "large_file_500MB",
			fileSize:    500 * 1024 * 1024, // 500MB
			expectedMin: 10 * 1024 * 1024,  // Should be at least 10MB
			expectedMax: 64 * 1024 * 1024,  // Should scale up for better throughput
			description: "Large files should use larger part sizes for throughput",
		},
		{
			name:        "extra_large_file_5GB",
			fileSize:    5 * 1024 * 1024 * 1024, // 5GB
			expectedMin: 32 * 1024 * 1024,       // Should use larger parts
			expectedMax: 128 * 1024 * 1024,      // But not too large
			description: "Extra large files should balance part count vs throughput",
		},
		{
			name:        "huge_file_50GB",
			fileSize:    50 * 1024 * 1024 * 1024, // 50GB
			expectedMin: 64 * 1024 * 1024,        // Must be large enough to stay under 10k parts
			expectedMax: 256 * 1024 * 1024,       // But reasonable for memory usage
			description: "Huge files must respect 10,000 part limit",
		},
		{
			name:        "pyspark_size_317MB",
			fileSize:    317 * 1024 * 1024, // 317MB (real-world pyspark example)
			expectedMin: 10 * 1024 * 1024,  // At least 10MB
			expectedMax: 64 * 1024 * 1024,  // Should use optimized size
			description: "Real-world pyspark file should have optimized part size",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			partSize := storage.calculateOptimalPartSize(tt.fileSize)

			// Verify part size is within expected range
			assert.GreaterOrEqual(t, partSize, tt.expectedMin,
				"Part size should be at least %d bytes for %s", tt.expectedMin, tt.description)
			assert.LessOrEqual(t, partSize, tt.expectedMax,
				"Part size should be at most %d bytes for %s", tt.expectedMax, tt.description)

			// Verify AWS S3 constraints
			assert.GreaterOrEqual(t, partSize, int64(5*1024*1024),
				"Part size must meet AWS S3 minimum of 5MB")

			// Verify part count doesn't exceed AWS limit
			partCount := (tt.fileSize + partSize - 1) / partSize // Ceiling division
			assert.LessOrEqual(t, partCount, int64(10000),
				"Part count (%d) must not exceed AWS S3 limit of 10,000 parts", partCount)

			t.Logf("File size: %dMB, Part size: %dMB, Part count: %d",
				tt.fileSize/(1024*1024), partSize/(1024*1024), partCount)
		})
	}
}

// TestS3Storage_CalculateOptimalPartSize_EdgeCases tests edge cases
func TestS3Storage_CalculateOptimalPartSize_EdgeCases(t *testing.T) {
	cfg := &S3Config{
		Endpoint: "test.endpoint",
		Bucket:   "test-bucket",
		PartSize: 10 * 1024 * 1024,
	}
	storage, err := NewS3Storage(cfg)
	if err != nil {
		t.Skipf("Cannot create S3 storage for testing: %v", err)
	}
	defer func() { _ = storage.Close() }()

	edgeCases := []struct {
		name        string
		fileSize    int64
		expectError bool
		description string
	}{
		{
			name:        "zero_size",
			fileSize:    0,
			expectError: false,
			description: "Zero size should not crash, should return minimum",
		},
		{
			name:        "negative_size",
			fileSize:    -1,
			expectError: false,
			description: "Negative size should not crash, should return minimum",
		},
		{
			name:        "very_small_size",
			fileSize:    1024, // 1KB
			expectError: false,
			description: "Very small files should use minimum part size",
		},
		{
			name:        "exact_aws_minimum",
			fileSize:    5 * 1024 * 1024, // Exact 5MB
			expectError: false,
			description: "Exact AWS minimum should work correctly",
		},
	}

	for _, tt := range edgeCases {
		t.Run(tt.name, func(t *testing.T) {
			// Should not panic
			partSize := storage.calculateOptimalPartSize(tt.fileSize)

			// Should always return at least AWS minimum
			assert.GreaterOrEqual(t, partSize, int64(5*1024*1024),
				"Even edge cases should return at least AWS S3 minimum part size")

			t.Logf("File size: %d bytes, Part size: %dMB", tt.fileSize, partSize/(1024*1024))
		})
	}
}

// TestS3Storage_CalculateOptimalPartSize_Performance tests performance characteristics
func TestS3Storage_CalculateOptimalPartSize_Performance(t *testing.T) {
	cfg := &S3Config{
		Endpoint: "test.endpoint",
		Bucket:   "test-bucket",
		PartSize: 10 * 1024 * 1024,
	}
	storage, err := NewS3Storage(cfg)
	if err != nil {
		t.Skipf("Cannot create S3 storage for testing: %v", err)
	}
	defer func() { _ = storage.Close() }()

	// Test that calculation is fast and consistent
	fileSize := int64(317 * 1024 * 1024) // pyspark size

	// Run multiple times to ensure consistency
	var results []int64
	for i := 0; i < 100; i++ {
		result := storage.calculateOptimalPartSize(fileSize)
		results = append(results, result)
	}

	// All results should be identical (deterministic)
	for i := 1; i < len(results); i++ {
		assert.Equal(t, results[0], results[i],
			"calculateOptimalPartSize should be deterministic")
	}

	t.Logf("Calculated part size for %dMB file: %dMB",
		fileSize/(1024*1024), results[0]/(1024*1024))
}
