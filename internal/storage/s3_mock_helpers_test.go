package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
)

// MockS3Client provides a mock implementation of MinIO client for testing
type MockS3Client struct {
	mu sync.RWMutex

	// Storage for mock objects
	objects map[string]*MockS3Object

	// Configuration
	bucket       string
	bucketExists bool

	// Error injection
	getObjectError    error
	putObjectError    error
	removeObjectError error
	statObjectError   error
	listObjectsError  error
	presignedURLError error
	bucketExistsError error

	// Operation counters for testing
	getObjectCalls    int
	putObjectCalls    int
	removeObjectCalls int
	statObjectCalls   int
	listObjectsCalls  int
	presignedURLCalls int
	bucketExistsCalls int
}

// MockS3Object represents a stored object in the mock client
type MockS3Object struct {
	Key          string
	Content      []byte
	ContentType  string
	LastModified time.Time
	ETag         string
	Metadata     map[string]string
}

// NewMockS3Client creates a new mock S3 client
func NewMockS3Client(bucket string) *MockS3Client {
	return &MockS3Client{
		objects:      make(map[string]*MockS3Object),
		bucket:       bucket,
		bucketExists: true,
	}
}

// SetBucketExists sets whether the bucket exists
func (m *MockS3Client) SetBucketExists(exists bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bucketExists = exists
}

// InjectError injects errors for specific operations
func (m *MockS3Client) InjectError(operation string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	switch operation {
	case "GetObject":
		m.getObjectError = err
	case "PutObject":
		m.putObjectError = err
	case "RemoveObject":
		m.removeObjectError = err
	case "StatObject":
		m.statObjectError = err
	case "ListObjects":
		m.listObjectsError = err
	case "PresignedURL":
		m.presignedURLError = err
	case "BucketExists":
		m.bucketExistsError = err
	}
}

// ClearErrors clears all injected errors
func (m *MockS3Client) ClearErrors() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.getObjectError = nil
	m.putObjectError = nil
	m.removeObjectError = nil
	m.statObjectError = nil
	m.listObjectsError = nil
	m.presignedURLError = nil
	m.bucketExistsError = nil
}

// GetOperationCounts returns the number of times each operation was called
func (m *MockS3Client) GetOperationCounts() map[string]int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return map[string]int{
		"GetObject":    m.getObjectCalls,
		"PutObject":    m.putObjectCalls,
		"RemoveObject": m.removeObjectCalls,
		"StatObject":   m.statObjectCalls,
		"ListObjects":  m.listObjectsCalls,
		"PresignedURL": m.presignedURLCalls,
		"BucketExists": m.bucketExistsCalls,
	}
}

// Reset clears all stored objects and resets counters
func (m *MockS3Client) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.objects = make(map[string]*MockS3Object)
	m.getObjectCalls = 0
	m.putObjectCalls = 0
	m.removeObjectCalls = 0
	m.statObjectCalls = 0
	m.listObjectsCalls = 0
	m.presignedURLCalls = 0
	m.bucketExistsCalls = 0
	m.ClearErrors()
}

// BucketExists mocks the BucketExists operation
func (m *MockS3Client) BucketExists(ctx context.Context, bucketName string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.bucketExistsCalls++

	if m.bucketExistsError != nil {
		return false, m.bucketExistsError
	}

	return m.bucketExists, nil
}

// GetObject mocks the GetObject operation
func (m *MockS3Client) GetObject(ctx context.Context, bucketName, objectName string, opts minio.GetObjectOptions) (*minio.Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.getObjectCalls++

	if m.getObjectError != nil {
		return nil, m.getObjectError
	}

	obj, exists := m.objects[objectName]
	if !exists {
		return nil, errors.New("NoSuchKey: The specified key does not exist")
	}

	// This is a simplified mock - real minio.Object is more complex
	// Note: obj.Content would be used for range requests in a full implementation
	_ = obj.Content
	return &minio.Object{}, nil
}

// PutObject mocks the PutObject operation
func (m *MockS3Client) PutObject(ctx context.Context, bucketName, objectName string, reader io.Reader, objectSize int64, opts minio.PutObjectOptions) (minio.UploadInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.putObjectCalls++

	if m.putObjectError != nil {
		return minio.UploadInfo{}, m.putObjectError
	}

	// Read the content
	content, err := io.ReadAll(reader)
	if err != nil {
		return minio.UploadInfo{}, err
	}

	// Generate ETag (simplified)
	etag := fmt.Sprintf("mock-etag-%x", len(content))

	// Store the object
	m.objects[objectName] = &MockS3Object{
		Key:          objectName,
		Content:      content,
		ContentType:  opts.ContentType,
		LastModified: time.Now(),
		ETag:         etag,
		Metadata:     opts.UserMetadata,
	}

	return minio.UploadInfo{
		Bucket: bucketName,
		Key:    objectName,
		ETag:   etag,
		Size:   int64(len(content)),
	}, nil
}

// RemoveObject mocks the RemoveObject operation
func (m *MockS3Client) RemoveObject(ctx context.Context, bucketName, objectName string, opts minio.RemoveObjectOptions) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.removeObjectCalls++

	if m.removeObjectError != nil {
		return m.removeObjectError
	}

	// S3 delete is idempotent - no error if object doesn't exist
	delete(m.objects, objectName)
	return nil
}

// StatObject mocks the StatObject operation
func (m *MockS3Client) StatObject(ctx context.Context, bucketName, objectName string, opts minio.StatObjectOptions) (minio.ObjectInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	m.statObjectCalls++

	if m.statObjectError != nil {
		return minio.ObjectInfo{}, m.statObjectError
	}

	obj, exists := m.objects[objectName]
	if !exists {
		return minio.ObjectInfo{}, errors.New("NoSuchKey: The specified key does not exist")
	}

	return minio.ObjectInfo{
		Key:          objectName,
		Size:         int64(len(obj.Content)),
		LastModified: obj.LastModified,
		ETag:         obj.ETag,
		ContentType:  obj.ContentType,
		UserMetadata: obj.Metadata,
	}, nil
}

// ListObjects mocks the ListObjects operation
func (m *MockS3Client) ListObjects(ctx context.Context, bucketName string, opts minio.ListObjectsOptions) <-chan minio.ObjectInfo {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.listObjectsCalls++

	objectCh := make(chan minio.ObjectInfo, 100)

	go func() {
		defer close(objectCh)

		if m.listObjectsError != nil {
			objectCh <- minio.ObjectInfo{Err: m.listObjectsError}
			return
		}

		count := 0
		for key, obj := range m.objects {
			// Apply prefix filter
			if opts.Prefix != "" && !strings.HasPrefix(key, opts.Prefix) {
				continue
			}

			// Apply StartAfter filter
			if opts.StartAfter != "" && key <= opts.StartAfter {
				continue
			}

			// Apply MaxKeys limit
			if opts.MaxKeys > 0 && count >= opts.MaxKeys {
				break
			}

			objectCh <- minio.ObjectInfo{
				Key:          key,
				Size:         int64(len(obj.Content)),
				LastModified: obj.LastModified,
				ETag:         obj.ETag,
				ContentType:  obj.ContentType,
			}

			count++
		}
	}()

	return objectCh
}

// PresignedGetObject mocks the PresignedGetObject operation
func (m *MockS3Client) PresignedGetObject(ctx context.Context, bucketName, objectName string, expiry time.Duration, reqParams map[string][]string) (*strings.Reader, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.presignedURLCalls++

	if m.presignedURLError != nil {
		return nil, m.presignedURLError
	}

	// Generate mock presigned URL
	url := fmt.Sprintf("https://mock-s3.example.com/%s/%s?mock-signature=abc123", bucketName, objectName)
	return strings.NewReader(url), nil
}

// MockMinioObject provides a mock minio.Object for testing
type MockMinioObject struct {
	reader io.Reader
	info   minio.ObjectInfo
	closed bool
}

// Read implements io.Reader
func (m *MockMinioObject) Read(p []byte) (n int, err error) {
	if m.closed {
		return 0, errors.New("object already closed")
	}
	return m.reader.Read(p)
}

// Close implements io.Closer
func (m *MockMinioObject) Close() error {
	m.closed = true
	return nil
}

// Stat returns object info
func (m *MockMinioObject) Stat() (minio.ObjectInfo, error) {
	return m.info, nil
}

// TestDataGenerator provides utilities for generating test data
type TestDataGenerator struct{}

// GenerateTestData creates test data of specified size with predictable content
func (g *TestDataGenerator) GenerateTestData(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 256)
	}
	return data
}

// GenerateRandomTestData creates test data with pseudo-random content
func (g *TestDataGenerator) GenerateRandomTestData(size int, seed int) []byte {
	data := make([]byte, size)
	rng := seed
	for i := range data {
		rng = (rng*1103515245 + 12345) & 0x7fffffff // Simple LCG
		data[i] = byte(rng % 256)
	}
	return data
}

// GenerateTextData creates text-based test data
func (g *TestDataGenerator) GenerateTextData(size int) []byte {
	text := "Lorem ipsum dolor sit amet, consectetur adipiscing elit, sed do eiusmod tempor incididunt ut labore et dolore magna aliqua. "

	var buf bytes.Buffer
	for buf.Len() < size {
		buf.WriteString(text)
	}

	data := buf.Bytes()
	if len(data) > size {
		data = data[:size]
	}

	return data
}

// GenerateBinaryData creates binary test data with all byte values
func (g *TestDataGenerator) GenerateBinaryData() []byte {
	data := make([]byte, 256)
	for i := range data {
		data[i] = byte(i)
	}
	return data
}

// GenerateUnicodeData creates Unicode test data
func (g *TestDataGenerator) GenerateUnicodeData() []byte {
	unicodeText := "Hello 世界! 🌍 Unicode test: 日本語, العربية, русский, हिन्दी, 한국어"
	return []byte(unicodeText)
}

// AssertionHelpers provides utilities for test assertions
type AssertionHelpers struct{}

// AssertObjectInfo validates ObjectInfo fields
func (h *AssertionHelpers) AssertObjectInfo(t interface {
	Errorf(format string, args ...interface{})
	Helper()
}, info *ObjectInfo, expectedKey string, expectedSize int64) {
	if helper, ok := t.(interface{ Helper() }); ok {
		helper.Helper()
	}

	if info == nil {
		t.Errorf("ObjectInfo should not be nil")
		return
	}

	if info.Key != expectedKey {
		t.Errorf("Expected key %s, got %s", expectedKey, info.Key)
	}

	if info.Size != expectedSize {
		t.Errorf("Expected size %d, got %d", expectedSize, info.Size)
	}

	if info.LastModified.IsZero() {
		t.Errorf("LastModified should not be zero")
	}
}

// AssertDataEqual compares two byte slices
func (h *AssertionHelpers) AssertDataEqual(t interface {
	Errorf(format string, args ...interface{})
	Helper()
}, expected, actual []byte, description string) {
	if helper, ok := t.(interface{ Helper() }); ok {
		helper.Helper()
	}

	if len(expected) != len(actual) {
		t.Errorf("%s: length mismatch - expected %d, got %d", description, len(expected), len(actual))
		return
	}

	for i, expectedByte := range expected {
		if actual[i] != expectedByte {
			t.Errorf("%s: byte mismatch at position %d - expected 0x%02x, got 0x%02x",
				description, i, expectedByte, actual[i])
			return
		}
	}
}

// AssertErrorContains checks if error contains expected substring
func (h *AssertionHelpers) AssertErrorContains(t interface {
	Errorf(format string, args ...interface{})
	Helper()
}, err error, expectedSubstring string) {
	if helper, ok := t.(interface{ Helper() }); ok {
		helper.Helper()
	}

	if err == nil {
		t.Errorf("Expected error containing '%s', but got no error", expectedSubstring)
		return
	}

	if !strings.Contains(err.Error(), expectedSubstring) {
		t.Errorf("Expected error to contain '%s', but got: %v", expectedSubstring, err)
	}
}

// TestScenarioBuilder helps build complex test scenarios
type TestScenarioBuilder struct {
	mockClient *MockS3Client
	storage    *S3Storage
	generator  *TestDataGenerator
	helpers    *AssertionHelpers
}

// NewTestScenarioBuilder creates a new test scenario builder
func NewTestScenarioBuilder(bucket string) *TestScenarioBuilder {
	mockClient := NewMockS3Client(bucket)
	generator := &TestDataGenerator{}
	helpers := &AssertionHelpers{}

	return &TestScenarioBuilder{
		mockClient: mockClient,
		generator:  generator,
		helpers:    helpers,
	}
}

// WithStorage sets up S3Storage with the mock client
func (b *TestScenarioBuilder) WithStorage(prefix string) *TestScenarioBuilder {
	// Note: This is a simplified approach. In real testing, we'd need to
	// inject the mock client into S3Storage, which would require refactoring
	// the S3Storage to accept an interface instead of concrete minio.Client
	b.storage = &S3Storage{
		bucket: b.mockClient.bucket,
		prefix: prefix,
	}
	return b
}

// AddTestObject adds a test object to the mock storage
func (b *TestScenarioBuilder) AddTestObject(key string, size int, contentType string) *TestScenarioBuilder {
	data := b.generator.GenerateTestData(size)
	b.mockClient.objects[key] = &MockS3Object{
		Key:          key,
		Content:      data,
		ContentType:  contentType,
		LastModified: time.Now(),
		ETag:         fmt.Sprintf("etag-%x", len(data)),
	}
	return b
}

// InjectError injects an error for testing error scenarios
func (b *TestScenarioBuilder) InjectError(operation string, err error) *TestScenarioBuilder {
	b.mockClient.InjectError(operation, err)
	return b
}

// GetMockClient returns the mock client for assertions
func (b *TestScenarioBuilder) GetMockClient() *MockS3Client {
	return b.mockClient
}

// GetStorage returns the storage instance
func (b *TestScenarioBuilder) GetStorage() *S3Storage {
	return b.storage
}

// GetGenerator returns the test data generator
func (b *TestScenarioBuilder) GetGenerator() *TestDataGenerator {
	return b.generator
}

// GetHelpers returns the assertion helpers
func (b *TestScenarioBuilder) GetHelpers() *AssertionHelpers {
	return b.helpers
}

// Reset resets the scenario for reuse
func (b *TestScenarioBuilder) Reset() *TestScenarioBuilder {
	b.mockClient.Reset()
	return b
}
