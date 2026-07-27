package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"golang.org/x/sync/singleflight"

	"github.com/huyhandes/groxpi/internal/telemetry"
)

// S3Config holds S3 storage configuration
type S3Config struct {
	Endpoint        string
	AccessKeyID     string
	SecretAccessKey string
	Region          string
	Bucket          string
	Prefix          string
	UseSSL          bool
	ForcePathStyle  bool

	// Performance tuning
	ConnectTimeout time.Duration
	RequestTimeout time.Duration
	EnableHTTP2    bool // Enable HTTP/2 for better multiplexing (default: true)
}

// s3MaxConns bounds the single HTTP connection pool shared by every S3 operation.
const s3MaxConns = 100

// newS3Transport builds the one HTTP transport all S3 operations share.
func newS3Transport(cfg *S3Config) *http.Transport {
	return &http.Transport{
		MaxIdleConns:          s3MaxConns,
		MaxIdleConnsPerHost:   s3MaxConns,
		MaxConnsPerHost:       s3MaxConns,
		IdleConnTimeout:       90 * time.Second,
		DisableCompression:    true, // S3 handles compression
		ResponseHeaderTimeout: cfg.RequestTimeout,
		TLSHandshakeTimeout:   cfg.ConnectTimeout,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     cfg.EnableHTTP2,
		DialContext: (&net.Dialer{
			Timeout:   cfg.ConnectTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}
}

// S3Storage implements Storage for S3-compatible backends.
//
// Its objects live across the network rather than on the local filesystem, so it
// deliberately does not implement ZeroCopyCapable: there is no local path to
// serve.
type S3Storage struct {
	client    *s3.Client
	uploader  *transfermanager.Client
	transport *http.Transport
	bucket    string
	prefix    string

	// existsSF deduplicates concurrent metadata lookups
	existsSF singleflight.Group
}

var (
	_ Storage       = (*S3Storage)(nil)
	_ PrefixDeleter = (*S3Storage)(nil)
)

// endpointURL turns a bare host, or a host that already carries a scheme, into
// the absolute URL the SDK wants as a base endpoint.
func endpointURL(endpoint string, useSSL bool) string {
	if strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://") {
		return endpoint
	}
	if useSSL {
		return "https://" + endpoint
	}
	return "http://" + endpoint
}

// NewS3Storage creates a new S3 storage backend.
//
// Credentials come from the AWS default chain — environment, shared config,
// instance metadata, container credentials, web identity — unless a static key
// pair is configured explicitly. Nothing here requires static keys, which is
// what makes instance and task roles usable.
func NewS3Storage(cfg *S3Config) (*S3Storage, error) {
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = 10 * time.Second
	}
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = 5 * time.Minute
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	// An explicit scheme on the endpoint wins over the UseSSL flag.
	if strings.HasPrefix(cfg.Endpoint, "https://") {
		cfg.UseSSL = true
	} else if strings.HasPrefix(cfg.Endpoint, "http://") {
		cfg.UseSSL = false
	}

	transport := newS3Transport(cfg)

	loadOpts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
		awsconfig.WithHTTPClient(&http.Client{Transport: transport}),
	}
	if cfg.AccessKeyID != "" && cfg.SecretAccessKey != "" {
		loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		))
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.ConnectTimeout)
	defer cancel()

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS configuration: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(endpointURL(cfg.Endpoint, cfg.UseSSL))
		}
		// MinIO cannot resolve virtual-hosted bucket names, and older releases
		// reject the checksum trailers the SDK now emits by default.
		o.UsePathStyle = cfg.ForcePathStyle
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})

	// Confirm the bucket is reachable before serving traffic.
	if _, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(cfg.Bucket)}); err != nil {
		return nil, fmt.Errorf("failed to reach bucket %s: %w", cfg.Bucket, err)
	}

	slog.Info("S3 storage backend initialized",
		"endpoint", cfg.Endpoint,
		"bucket", cfg.Bucket,
		"prefix", cfg.Prefix,
		"path_style", cfg.ForcePathStyle,
		"static_credentials", cfg.AccessKeyID != "")

	return &S3Storage{
		client: client,
		// The transfer manager keeps its own checksum setting, which overrides
		// the client's, so it has to be pinned here too.
		uploader: transfermanager.New(client, func(o *transfermanager.Options) {
			o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		}),
		transport: transport,
		bucket:    cfg.Bucket,
		prefix:    strings.TrimSuffix(cfg.Prefix, "/"),
	}, nil
}

// buildKey constructs the full S3 key with prefix
func (s *S3Storage) buildKey(key string) string {
	if s.prefix == "" {
		return key
	}
	return fmt.Sprintf("%s/%s", s.prefix, key)
}

// isNotFoundResponse reports whether err is the SDK's way of saying the object
// is simply absent, as opposed to any other failure (denied, throttled,
// network). Only this case may be reported as a miss.
func isNotFoundResponse(err error) bool {
	var noSuchKey *types.NoSuchKey
	var notFound *types.NotFound
	if errors.As(err, &noSuchKey) || errors.As(err, &notFound) {
		return true
	}

	// HeadObject on a missing key answers with a bodyless 404, which the SDK
	// deserializes into an unmodelled API error whose code it derives from the
	// status line. Only the codes that mean "this object" may be folded into a
	// miss: a 404 also covers NoSuchBucket, and reading a deleted bucket as an
	// absent object turns a broken configuration into a silent permanent 100%
	// miss - every read re-downloads and every write is thrown away.
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.ErrorCode() {
	case "NoSuchKey", "NotFound":
		return true
	default:
		return false
	}
}

// s3Error wraps a backend failure for key, folding a genuine absence into the
// shared ErrNotFound sentinel so callers can branch with errors.Is.
func s3Error(err error, key string) error {
	if isNotFoundResponse(err) {
		return fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	return fmt.Errorf("s3 operation on %s failed: %w", key, err)
}

// Get retrieves an object from S3.
//
// Reads are deliberately not deduplicated: an S3 reader can only be consumed
// once, so concurrent callers each need their own.
func (s *S3Storage) Get(ctx context.Context, key string) (io.ReadCloser, *ObjectInfo, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.buildKey(key)),
	})
	if err != nil {
		return nil, nil, s3Error(err, key)
	}

	// The response headers are complete before any byte of the body is read,
	// so the caller can emit its own headers and only then copy.
	info := &ObjectInfo{
		Key:         key,
		Size:        aws.ToInt64(out.ContentLength),
		ETag:        aws.ToString(out.ETag),
		ContentType: aws.ToString(out.ContentType),
	}
	if out.LastModified != nil {
		info.LastModified = *out.LastModified
	}

	return out.Body, info, nil
}

// Put stores an object in S3.
//
// A seekable body of known size is a plain single-object put; anything else — a
// live download, a pipe — goes through the transfer manager, the only path able
// to upload a stream whose length is not known up front.
func (s *S3Storage) Put(ctx context.Context, key string, reader io.Reader, size int64, contentType string) (*ObjectInfo, error) {
	fullKey := s.buildKey(key)

	var etag string
	if seeker, ok := reader.(io.ReadSeeker); ok && size >= 0 {
		out, err := s.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(s.bucket),
			Key:           aws.String(fullKey),
			Body:          seeker,
			ContentLength: aws.Int64(size),
			ContentType:   aws.String(contentType),
		})
		if err != nil {
			return nil, fmt.Errorf("failed to put object %s: %w", key, err)
		}
		etag = aws.ToString(out.ETag)
	} else {
		counter := &countingReader{r: reader}
		out, err := s.uploader.UploadObject(ctx, &transfermanager.UploadObjectInput{
			Bucket:      aws.String(s.bucket),
			Key:         aws.String(fullKey),
			Body:        counter,
			ContentType: aws.String(contentType),
		})
		if err != nil {
			return nil, fmt.Errorf("failed to put object %s: %w", key, err)
		}
		etag = aws.ToString(out.ETag)
		size = counter.n
	}

	slog.Debug("Object stored in S3", "key", key, "size", size)

	return &ObjectInfo{
		Key:         key,
		Size:        size,
		ETag:        etag,
		ContentType: contentType,
	}, nil
}

// countingReader reports how many bytes went out when the caller could not say
// up front.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// Delete removes an object from S3
func (s *S3Storage) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.buildKey(key)),
	})
	if err != nil {
		return fmt.Errorf("failed to delete object %s: %w", key, err)
	}
	return nil
}

// deleteBatchSize is the most keys S3 accepts in one DeleteObjects request.
const deleteBatchSize = 1000

// DeletePrefix removes every object under prefix and reports how many went. It is
// what makes evicting a package mean anything in pure-S3 mode: the transport
// evicts only through PrefixDeleter, so without this the admin endpoint answered
// 200 having deleted nothing.
//
// Listing and deleting are interleaved page by page rather than collected first,
// so evicting a package with thousands of files costs one page of keys in memory
// instead of all of them.
func (s *S3Storage) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	pages := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket:  aws.String(s.bucket),
		Prefix:  aws.String(s.buildKey(prefix)),
		MaxKeys: aws.Int32(deleteBatchSize),
	})

	deleted := 0
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return deleted, fmt.Errorf("failed to list objects under %s: %w", prefix, err)
		}
		if len(page.Contents) == 0 {
			continue
		}

		ids := make([]types.ObjectIdentifier, 0, len(page.Contents))
		for _, object := range page.Contents {
			ids = append(ids, types.ObjectIdentifier{Key: object.Key})
		}

		out, err := s.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(s.bucket),
			// Quiet mode answers with the failures only, which is also what makes
			// the response size independent of the batch size.
			Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return deleted, fmt.Errorf("failed to delete objects under %s: %w", prefix, err)
		}

		deleted += len(ids) - len(out.Errors)
		if len(out.Errors) > 0 {
			return deleted, fmt.Errorf("failed to delete %d of %d objects under %s: %s",
				len(out.Errors), len(ids), prefix, aws.ToString(out.Errors[0].Message))
		}
	}

	slog.Debug("Deleted object prefix from S3", "prefix", prefix, "deleted", deleted)

	return deleted, nil
}

// Exists checks if an object exists in S3, deduplicating concurrent lookups.
func (s *S3Storage) Exists(ctx context.Context, key string) (bool, error) {
	// A caller who arrived with a dead context is answered from its own context,
	// never by starting or joining a flight.
	if err := ctx.Err(); err != nil {
		return false, err
	}

	result, err, _ := s.existsSF.Do(key, func() (any, error) {
		// The flight is shared, so it must not be cancellable by whichever caller
		// happened to start it: one client disconnecting used to fail every other
		// caller waiting on the same key with context.Canceled, which the read path
		// reads as a miss and re-downloads a file that is sitting in the bucket.
		// Values (the trace context) are kept; the deadline comes from the
		// transport's response-header timeout instead.
		_, err := s.client.HeadObject(context.WithoutCancel(ctx), &s3.HeadObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(s.buildKey(key)),
		})
		if err != nil {
			// Absence is the answer, not a failure. Anything else is a failure
			// and must not be reported as "does not exist".
			if isNotFoundResponse(err) {
				return false, nil
			}
			return false, fmt.Errorf("failed to check object existence %s: %w", key, err)
		}
		return true, nil
	})
	if err != nil {
		return false, err
	}

	// Counted per caller rather than per flight: every caller that got an answer
	// out of this either hit or missed, whether or not it did the lookup.
	exists := result.(bool)
	if exists {
		telemetry.CacheHit(ctx, telemetry.LayerRemote)
	} else {
		telemetry.CacheMiss(ctx, telemetry.LayerRemote)
	}

	return exists, nil
}

// Close releases any resources held by the storage backend
func (s *S3Storage) Close() error {
	s.transport.CloseIdleConnections()
	return nil
}
