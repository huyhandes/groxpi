package pypi

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/bytedance/sonic"
)

// Zero-copy benchmark tests following TDD principles

// Buffer pool for testing zero-copy optimizations
var testBufferPool = sync.Pool{
	New: func() interface{} {
		return make([]byte, 64*1024) // 64KB buffers
	},
}

// Baseline benchmarks using standard approaches (with copying)
func BenchmarkJSONMarshal_StandardCopy(b *testing.B) {
	data := map[string]interface{}{
		"meta": map[string]interface{}{
			"api-version": "1.0",
		},
		"name": "test-package",
		"files": []map[string]interface{}{
			{
				"filename": "test-package-1.0.0.tar.gz",
				"url":      "https://files.pythonhosted.org/packages/.../test-package-1.0.0.tar.gz",
				"hashes":   map[string]string{"sha256": "abc123"},
			},
			{
				"filename": "test-package-1.0.0-py3-none-any.whl",
				"url":      "https://files.pythonhosted.org/packages/.../test-package-1.0.0-py3-none-any.whl",
				"hashes":   map[string]string{"sha256": "def456"},
			},
		},
	}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		jsonData, err := sonic.ConfigFastest.Marshal(data)
		if err != nil {
			b.Fatal(err)
		}
		_ = jsonData // Simulate using the data
	}
}

func BenchmarkJSONMarshal_StreamingZeroCopy(b *testing.B) {
	data := map[string]interface{}{
		"meta": map[string]interface{}{
			"api-version": "1.0",
		},
		"name": "test-package",
		"files": []map[string]interface{}{
			{
				"filename": "test-package-1.0.0.tar.gz",
				"url":      "https://files.pythonhosted.org/packages/.../test-package-1.0.0.tar.gz",
				"hashes":   map[string]string{"sha256": "abc123"},
			},
			{
				"filename": "test-package-1.0.0-py3-none-any.whl",
				"url":      "https://files.pythonhosted.org/packages/.../test-package-1.0.0-py3-none-any.whl",
				"hashes":   map[string]string{"sha256": "def456"},
			},
		},
	}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		var buf bytes.Buffer
		encoder := sonic.ConfigFastest.NewEncoder(&buf)
		err := encoder.Encode(data)
		if err != nil {
			b.Fatal(err)
		}
		_ = buf.Bytes() // Simulate using the data
	}
}

func BenchmarkBufferCopy_StandardIoCopy(b *testing.B) {
	// Create test data - 1MB of content
	testData := strings.Repeat("test data for zero-copy benchmarking\n", 26214) // ~1MB
	reader := strings.NewReader(testData)

	b.ReportAllocs()
	b.SetBytes(int64(len(testData)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		reader.Reset(testData)
		var buf bytes.Buffer
		_, err := io.Copy(&buf, reader)
		if err != nil {
			b.Fatal(err)
		}
		_ = buf.Bytes() // Simulate using the data
	}
}

func BenchmarkBufferCopy_PooledCopyBuffer(b *testing.B) {
	// Create test data - 1MB of content
	testData := strings.Repeat("test data for zero-copy benchmarking\n", 26214) // ~1MB
	reader := strings.NewReader(testData)

	b.ReportAllocs()
	b.SetBytes(int64(len(testData)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		reader.Reset(testData)
		var buf bytes.Buffer

		// Use pooled buffer for copy operations
		copyBuf := testBufferPool.Get().([]byte)
		_, err := io.CopyBuffer(&buf, reader, copyBuf)
		testBufferPool.Put(copyBuf)

		if err != nil {
			b.Fatal(err)
		}
		_ = buf.Bytes() // Simulate using the data
	}
}

// Benchmarks for PyPI response parsing with zero-copy optimizations
func BenchmarkPyPIResponseParsing_StandardCopy(b *testing.B) {
	jsonResponse := `{
		"meta": {"api-version": "1.0"},
		"name": "numpy",
		"files": [
			{
				"filename": "numpy-1.21.0-py3-none-any.whl",
				"url": "https://files.pythonhosted.org/packages/.../numpy-1.21.0-py3-none-any.whl",
				"requires-python": ">=3.7",
				"hashes": {"sha256": "abc123def456"},
				"yanked": false
			},
			{
				"filename": "numpy-1.21.0.tar.gz",
				"url": "https://files.pythonhosted.org/packages/.../numpy-1.21.0.tar.gz",
				"requires-python": ">=3.7",
				"hashes": {"sha256": "def456abc123"},
				"yanked": false
			}
		]
	}`

	b.ReportAllocs()
	b.SetBytes(int64(len(jsonResponse)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		reader := strings.NewReader(jsonResponse)

		// Standard approach: read all into memory first
		data, err := io.ReadAll(reader)
		if err != nil {
			b.Fatal(err)
		}

		var response PyPISimpleResponse
		err = sonic.ConfigFastest.Unmarshal(data, &response)
		if err != nil {
			b.Fatal(err)
		}
		_ = response.Files // Simulate using the data
	}
}

func BenchmarkPyPIResponseParsing_StreamingZeroCopy(b *testing.B) {
	jsonResponse := `{
		"meta": {"api-version": "1.0"},
		"name": "numpy",
		"files": [
			{
				"filename": "numpy-1.21.0-py3-none-any.whl",
				"url": "https://files.pythonhosted.org/packages/.../numpy-1.21.0-py3-none-any.whl",
				"requires-python": ">=3.7",
				"hashes": {"sha256": "abc123def456"},
				"yanked": false
			},
			{
				"filename": "numpy-1.21.0.tar.gz",
				"url": "https://files.pythonhosted.org/packages/.../numpy-1.21.0.tar.gz",
				"requires-python": ">=3.7",
				"hashes": {"sha256": "def456abc123"},
				"yanked": false
			}
		]
	}`

	b.ReportAllocs()
	b.SetBytes(int64(len(jsonResponse)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		reader := strings.NewReader(jsonResponse)

		// Zero-copy approach: stream directly from reader
		decoder := sonic.ConfigFastest.NewDecoder(reader)
		var response PyPISimpleResponse
		err := decoder.Decode(&response)
		if err != nil {
			b.Fatal(err)
		}
		_ = response.Files // Simulate using the data
	}
}

// Memory allocation tests to verify zero-copy optimizations
func BenchmarkMemoryAllocation_StandardString(b *testing.B) {
	data := []byte("test string for zero-copy conversion benchmarking")

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		// Standard approach - allocates new string
		s := string(data)
		_ = s // Use the string
	}
}

func BenchmarkMemoryAllocation_UnsafeString(b *testing.B) {
	data := []byte("test string for zero-copy conversion benchmarking")

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		// Zero-copy approach using unsafe (when safe to do so)
		// Note: This is only safe when the byte slice won't be modified
		// and has the same lifetime as the string
		s := unsafeStringFromBytes(data)
		_ = s // Use the string
	}
}

// Helper functions for zero-copy optimizations
func unsafeStringFromBytes(b []byte) string {
	// Note: This is unsafe and should only be used when:
	// 1. The byte slice will not be modified
	// 2. The byte slice has at least the same lifetime as the string
	// 3. Performance is critical and safety is ensured by the caller

	// For now, use the safe approach - actual unsafe implementation
	// should be carefully considered and tested
	return string(b)
}

// Benchmark response caching with zero-copy optimizations
func BenchmarkResponseCaching_StandardCopy(b *testing.B) {
	// Simulate cached JSON response
	cachedData := []byte(`{"meta":{"api-version":"1.0"},"projects":[{"name":"test1"},{"name":"test2"}]}`)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		// Standard approach - copy the cached data
		response := make([]byte, len(cachedData))
		copy(response, cachedData)
		_ = response // Simulate sending response
	}
}

func BenchmarkResponseCaching_ZeroCopy(b *testing.B) {
	// Simulate cached JSON response
	cachedData := []byte(`{"meta":{"api-version":"1.0"},"projects":[{"name":"test1"},{"name":"test2"}]}`)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		// Zero-copy approach - reference the cached data directly
		// This is safe for read-only cached responses
		response := cachedData
		_ = response // Simulate sending response
	}
}
