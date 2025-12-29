package streaming

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"unsafe"

	"github.com/phuslu/log"
)

// zeroCopyServer implements ZeroCopyServer interface
type zeroCopyServer struct {
	copyBufPool *sync.Pool
}

// NewZeroCopyServer creates a new ZeroCopyServer
func NewZeroCopyServer() ZeroCopyServer {
	return &zeroCopyServer{
		copyBufPool: &sync.Pool{
			New: func() interface{} {
				buf := make([]byte, 64*1024) // 64KB buffer
				return &buf
			},
		},
	}
}

// ServeFile serves a file using zero-copy techniques when possible
func (zcs *zeroCopyServer) ServeFile(ctx context.Context, writer io.Writer, filepath string) error {
	// Try to use sendfile syscall if writer supports it
	if tcpConn, ok := writer.(interface{ File() (*os.File, error) }); ok {
		return zcs.serveFileWithSendfile(ctx, tcpConn, filepath)
	}

	// Fall back to optimized copy
	file, err := os.Open(filepath)
	if err != nil {
		return fmt.Errorf("failed to open file %s: %w", filepath, err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			// Log error but continue
			_ = err
		}
	}()

	return zcs.ServeReader(ctx, writer, file, -1)
}

// ServeReader serves data from reader using optimized copy techniques
func (zcs *zeroCopyServer) ServeReader(ctx context.Context, writer io.Writer, reader io.Reader, size int64) error {
	// Use pooled buffer for efficient copying
	copyBufPtr := zcs.copyBufPool.Get().(*[]byte)
	defer zcs.copyBufPool.Put(copyBufPtr)
	copyBuf := *copyBufPtr

	// Check context for cancellation
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	_, err := io.CopyBuffer(writer, reader, copyBuf)
	return err
}

// serveFileWithSendfile uses the sendfile syscall for zero-copy file transfer
func (zcs *zeroCopyServer) serveFileWithSendfile(ctx context.Context, conn interface{ File() (*os.File, error) }, filepath string) error {
	// Get the connection file descriptor
	connFile, err := conn.File()
	if err != nil {
		log.Debug().Err(err).Msg("Failed to get connection file descriptor, falling back to regular copy")
		return zcs.serveFileRegular(ctx, conn.(io.Writer), filepath)
	}
	defer func() {
		if err := connFile.Close(); err != nil {
			// Log error but continue
			_ = err
		}
	}()

	// Open the source file
	srcFile, err := os.Open(filepath)
	if err != nil {
		return fmt.Errorf("failed to open source file: %w", err)
	}
	defer func() {
		if err := srcFile.Close(); err != nil {
			// Log error but continue
			_ = err
		}
	}()

	// Get file info for size
	stat, err := srcFile.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat file: %w", err)
	}

	// Use sendfile syscall for zero-copy transfer
	return zcs.sendfile(int(connFile.Fd()), int(srcFile.Fd()), stat.Size())
}

// serveFileRegular falls back to regular file serving
func (zcs *zeroCopyServer) serveFileRegular(ctx context.Context, writer io.Writer, filepath string) error {
	file, err := os.Open(filepath)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			// Log error but continue
			_ = err
		}
	}()

	return zcs.ServeReader(ctx, writer, file, -1)
}

// sendfile performs the actual sendfile syscall
func (zcs *zeroCopyServer) sendfile(dst, src int, size int64) error {
	// Use sendfile syscall on Linux
	var offset int64 = 0
	remaining := size

	for remaining > 0 {
		// sendfile syscall - platform specific
		n, err := syscall.Sendfile(dst, src, &offset, int(remaining))
		if err != nil {
			if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
				// Non-blocking socket would block, retry
				continue
			}
			return fmt.Errorf("sendfile failed: %w", err)
		}

		if n == 0 {
			break // EOF
		}

		remaining -= int64(n)
		offset += int64(n)
	}

	log.Debug().
		Int64("bytes_sent", size-remaining).
		Int64("total_size", size).
		Msg("Sendfile completed")

	return nil
}

// MemoryMappedServer provides memory-mapped file serving for very large files
type memoryMappedServer struct{}

// NewMemoryMappedServer creates a server that uses memory mapping for large files
func NewMemoryMappedServer() ZeroCopyServer {
	return &memoryMappedServer{}
}

// ServeFile serves a file using memory mapping
func (mms *memoryMappedServer) ServeFile(ctx context.Context, writer io.Writer, filepath string) error {
	file, err := os.Open(filepath)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			// Log error but continue
			_ = err
		}
	}()

	stat, err := file.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat file: %w", err)
	}

	size := stat.Size()
	if size == 0 {
		return nil
	}

	// Memory map the file
	data, err := syscall.Mmap(int(file.Fd()), 0, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return fmt.Errorf("failed to mmap file: %w", err)
	}
	defer func() { _ = syscall.Munmap(data) }()

	// Write memory-mapped data directly
	ptr := uintptr(unsafe.Pointer(&data[0]))
	slice := (*[]byte)(unsafe.Pointer(&struct {
		addr uintptr
		len  int
		cap  int
	}{ptr, len(data), len(data)}))

	_, err = writer.Write(*slice)
	return err
}

// ServeReader falls back to regular reader serving (mmap doesn't apply)
func (mms *memoryMappedServer) ServeReader(ctx context.Context, writer io.Writer, reader io.Reader, size int64) error {
	zcs := NewZeroCopyServer()
	return zcs.ServeReader(ctx, writer, reader, size)
}

// OptimalServer chooses the best serving strategy based on context
type optimalServer struct {
	sendfileServer ZeroCopyServer
	mmapServer     ZeroCopyServer
	regularServer  ZeroCopyServer
}

// NewOptimalServer creates a server that automatically chooses the best strategy
func NewOptimalServer() ZeroCopyServer {
	return &optimalServer{
		sendfileServer: NewZeroCopyServer(),
		mmapServer:     NewMemoryMappedServer(),
		regularServer:  NewZeroCopyServer(),
	}
}

// ServeFile automatically chooses optimal serving strategy
func (os *optimalServer) ServeFile(ctx context.Context, writer io.Writer, filepath string) error {
	// Get file info to decide strategy
	stat, err := os.Stat(filepath)
	if err != nil {
		return err
	}

	size := stat.Size()

	// Choose strategy based on size and writer type
	switch {
	case size > 100*1024*1024: // Files > 100MB - use memory mapping
		log.Debug().Str("file", filepath).Int64("size", size).Msg("Using memory-mapped serving")
		return os.mmapServer.ServeFile(ctx, writer, filepath)

	case supportsSendfile(writer): // TCP connection - use sendfile
		log.Debug().Str("file", filepath).Msg("Using sendfile serving")
		return os.sendfileServer.ServeFile(ctx, writer, filepath)

	default: // Regular optimized copy
		log.Debug().Str("file", filepath).Msg("Using regular optimized serving")
		return os.regularServer.ServeFile(ctx, writer, filepath)
	}
}

// ServeReader chooses optimal reader serving strategy
func (os *optimalServer) ServeReader(ctx context.Context, writer io.Writer, reader io.Reader, size int64) error {
	return os.regularServer.ServeReader(ctx, writer, reader, size)
}

// Stat gets file info using standard os.Stat
func (o *optimalServer) Stat(filepath string) (os.FileInfo, error) {
	return os.Stat(filepath)
}

// supportsSendfile checks if writer supports sendfile syscall
func supportsSendfile(writer io.Writer) bool {
	_, ok := writer.(interface{ File() (*os.File, error) })
	return ok
}
