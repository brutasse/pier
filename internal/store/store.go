// Package store is the S3-backed object storage for the repository.
package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/brutasse/pier/internal/config"
)

var (
	// ErrNotFound is returned when the object does not exist.
	ErrNotFound = errors.New("object not found")
	// ErrConflict is returned when an immutable object already exists.
	ErrConflict = errors.New("object already exists")
)

// Backend is the storage interface used by the API layer.
type Backend interface {
	// Get returns the object body. The caller must close the reader.
	Get(ctx context.Context, path string) (rc io.ReadCloser, size int64, contentType, etag string, err error)
	// GetRange returns the [start, end] byte range of the object
	// (inclusive). The caller must close the reader.
	GetRange(ctx context.Context, path string, start, end int64) (rc io.ReadCloser, err error)
	// Head reports the stored object's metadata. lastModified is when
	// the object was last written.
	Head(ctx context.Context, path string) (size int64, contentType, etag string, lastModified time.Time, err error)
	// List returns the paths of all objects stored under prefix (that
	// is, with the key prefix+"/" as their prefix).
	List(ctx context.Context, prefix string) ([]string, error)
	// Put stores the object. size is the exact body size, or -1 when
	// unknown.
	Put(ctx context.Context, path string, r io.Reader, size int64, contentType string, immutable bool) error
	Delete(ctx context.Context, path string) error
	// Ping verifies the configured bucket is reachable.
	Ping(ctx context.Context) error

	// Multipart upload primitives, used by the pull-through cache to
	// stream objects to the store without buffering them.
	// CreateMultipart starts an upload and returns its ID.
	CreateMultipart(ctx context.Context, path, contentType string) (uploadID string, err error)
	// UploadPart stores a part of the upload. size is the exact body size.
	UploadPart(ctx context.Context, path, uploadID string, partNum int32, r io.Reader, size int64) (etag string, err error)
	// CompleteMultipart finalizes the upload from its parts.
	CompleteMultipart(ctx context.Context, path, uploadID string, parts []Part) error
	// AbortMultipart discards an in-progress upload. It is best-effort:
	// callers may ignore errors.
	AbortMultipart(ctx context.Context, path, uploadID string) error
}

// Part is a completed part of a multipart upload.
type Part struct {
	Num  int32
	ETag string
	Size int64
}

// S3 is an S3-compatible Backend.
type S3 struct {
	client *s3.Client
	bucket string
	prefix string
}

// NewS3 builds the S3 backend from config. Credentials come from the
// standard AWS SDK sources (environment, shared file, instance profile).
func NewS3(ctx context.Context, cfg config.S3Config) (*S3, error) {
	awsCfg, err := awscfg.LoadDefaultConfig(ctx, awscfg.WithRegion(cfg.Region))
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	if cfg.Endpoint != "" {
		ep := strings.TrimRight(cfg.Endpoint, "/")
		if !strings.HasPrefix(ep, "http://") && !strings.HasPrefix(ep, "https://") {
			ep = "https://" + ep
		}
		awsCfg.BaseEndpoint = aws.String(ep)
	}
	var opt func(*s3.Options)
	if cfg.ForcePathStyleOrDefault() {
		opt = func(o *s3.Options) { o.UsePathStyle = true }
	}
	if opt == nil {
		return &S3{client: s3.NewFromConfig(awsCfg), bucket: cfg.Bucket, prefix: cfg.Prefix}, nil
	}
	return &S3{client: s3.NewFromConfig(awsCfg, opt), bucket: cfg.Bucket, prefix: cfg.Prefix}, nil
}

func (s *S3) Ping(ctx context.Context) error {
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(s.bucket),
	})
	if err != nil {
		return fmt.Errorf("s3 head bucket %q: %w", s.bucket, err)
	}
	return nil
}

func (s *S3) key(path string) string {
	if s.prefix == "" {
		return path
	}
	return s.prefix + "/" + path
}

func (s *S3) Get(ctx context.Context, path string) (io.ReadCloser, int64, string, string, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key(path)),
	})
	var noSuch *types.NoSuchKey
	if errors.As(err, &noSuch) {
		return nil, 0, "", "", ErrNotFound
	}
	if err != nil {
		return nil, 0, "", "", fmt.Errorf("s3 get %q: %w", path, err)
	}
	return out.Body, aws.ToInt64(out.ContentLength), aws.ToString(out.ContentType), aws.ToString(out.ETag), nil
}

func (s *S3) GetRange(ctx context.Context, path string, start, end int64) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key(path)),
		Range:  aws.String(fmt.Sprintf("bytes=%d-%d", start, end)),
	})
	var noSuch *types.NoSuchKey
	if errors.As(err, &noSuch) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("s3 get range %q (%d-%d): %w", path, start, end, err)
	}
	return out.Body, nil
}

func (s *S3) Head(ctx context.Context, path string) (int64, string, string, time.Time, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key(path)),
	})
	var notFound *types.NotFound
	if errors.As(err, &notFound) {
		return 0, "", "", time.Time{}, ErrNotFound
	}
	if err != nil {
		return 0, "", "", time.Time{}, fmt.Errorf("s3 head %q: %w", path, err)
	}
	lastModified := time.Time{}
	if out.LastModified != nil {
		lastModified = *out.LastModified
	}
	return aws.ToInt64(out.ContentLength), aws.ToString(out.ContentType), aws.ToString(out.ETag), lastModified, nil
}

func (s *S3) List(ctx context.Context, prefix string) ([]string, error) {
	full := s.key(prefix + "/")
	var out []string
	pg := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(full),
	})
	for pg.HasMorePages() {
		page, err := pg.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("s3 list %q: %w", prefix, err)
		}
		for _, o := range page.Contents {
			out = append(out, strings.TrimPrefix(aws.ToString(o.Key), full))
		}
	}
	return out, nil
}

func (s *S3) Put(ctx context.Context, path string, r io.Reader, size int64, contentType string, immutable bool) error {
	in := &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(s.key(path)),
		Body:        r,
		ContentType: aws.String(contentType),
	}
	if size >= 0 {
		in.ContentLength = aws.Int64(size)
	}
	if immutable {
		in.IfNoneMatch = aws.String("*")
	}
	_, err := s.client.PutObject(ctx, in)
	if isConflict(err) {
		return ErrConflict
	}
	if err != nil {
		return fmt.Errorf("s3 put %q: %w", path, err)
	}
	return nil
}

// isConflict reports whether err is a failed conditional put. S3 answers a
// failed If-None-Match with 409 Conflict; some S3-compatible stores answer
// with 412 Precondition Failed.
func isConflict(err error) bool {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.ErrorCode() {
	case "Conflict", "PreconditionFailed":
		return true
	}
	return false
}

func (s *S3) Delete(ctx context.Context, path string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key(path)),
	})
	if err != nil {
		return fmt.Errorf("s3 delete %q: %w", path, err)
	}
	return nil
}

func (s *S3) CreateMultipart(ctx context.Context, path, contentType string) (string, error) {
	out, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(s.key(path)),
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return "", fmt.Errorf("s3 create multipart %q: %w", path, err)
	}
	return aws.ToString(out.UploadId), nil
}

func (s *S3) UploadPart(ctx context.Context, path, uploadID string, partNum int32, r io.Reader, size int64) (string, error) {
	out, err := s.client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(s.key(path)),
		UploadId:      aws.String(uploadID),
		PartNumber:    aws.Int32(partNum),
		Body:          r,
		ContentLength: aws.Int64(size),
	})
	if err != nil {
		return "", fmt.Errorf("s3 upload part %q (%d): %w", path, partNum, err)
	}
	return aws.ToString(out.ETag), nil
}

func (s *S3) CompleteMultipart(ctx context.Context, path, uploadID string, parts []Part) error {
	var completed []types.CompletedPart
	for _, p := range parts {
		completed = append(completed, types.CompletedPart{
			ETag:       aws.String(p.ETag),
			PartNumber: aws.Int32(p.Num),
		})
	}
	_, err := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(s.key(path)),
		UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: completed,
		},
	})
	if err != nil {
		return fmt.Errorf("s3 complete multipart %q: %w", path, err)
	}
	return nil
}

func (s *S3) AbortMultipart(ctx context.Context, path, uploadID string) error {
	_, err := s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(s.key(path)),
		UploadId: aws.String(uploadID),
	})
	if err != nil {
		return fmt.Errorf("s3 abort multipart %q: %w", path, err)
	}
	return nil
}
