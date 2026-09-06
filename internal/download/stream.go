package download

// StreamResult is what one completed upstream download produced.
type StreamResult struct {
	Size        int64
	ContentType string
	ETag        string
	Error       error // cache-write failure; the client transfer still completed
}

// Expectation is what the index declared about a file, used to verify the
// bytes before they are committed to storage.
type Expectation struct {
	SHA256 string // hex digest, empty when the index supplied none
	Size   int64  // declared size in bytes, <= 0 when unknown
}
