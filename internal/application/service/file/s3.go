// Copyright (C) 2026 Tencent. All rights reserved.
// Originally part of WeKnora (https://github.com/Tencent/WeKnora), MIT
// License, by way of Omnisc (internal/application/service/file/s3.go and
// object_storage_http.go). Modified for Trelis: one S3-compatible provider
// configured from storage.s3, keys instead of s3://bucket/key paths, keys
// built from Document and upload IDs (ADR-0006); no tenants, path prefix,
// copies, temp files, presigned URLs or SSRF-guarded client (the endpoint
// is operator configuration). See NOTICE.

package file

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

const (
	// objectStorageSetupTimeout bounds bucket checks and creation at start.
	objectStorageSetupTimeout = 30 * time.Second
	// objectStorageTransferTimeout bounds one upload or download when the
	// caller's context has no deadline of its own.
	objectStorageTransferTimeout = 30 * time.Minute
	// deleteBatchSize is the most keys DeleteObjects accepts in one request.
	deleteBatchSize = 1000
)

// S3Options configures the S3-compatible store.
type S3Options struct {
	// Endpoint is the base URL of an S3-compatible server, e.g.
	// http://localhost:9000; empty means AWS S3 for Region.
	Endpoint string
	Region   string
	Bucket   string
	// AccessKeyID and SecretAccessKey go together; both empty keeps the AWS
	// default credential chain (IAM roles, environment, shared config).
	AccessKeyID     string
	SecretAccessKey string
	// ForcePathStyle addresses buckets as {endpoint}/{bucket}; otherwise
	// path-style is used for every endpoint that is not AWS.
	ForcePathStyle bool
}

// S3FileService implements FileService on one S3-compatible bucket.
type S3FileService struct {
	client     *s3.Client
	bucketName string
}

// s3UsePathStyle reports whether the S3 client should use path-style
// addressing. S3-compatible services default to path-style; AWS uses
// virtual-hosted style. force_path_style always wins.
func s3UsePathStyle(endpoint string, forcePathStyle bool) bool {
	return forcePathStyle || !isAWSEndpoint(endpoint)
}

// isAWSEndpoint reports whether endpoint is AWS S3 itself.
func isAWSEndpoint(endpoint string) bool {
	return strings.Contains(endpoint, "amazonaws.com")
}

// NewS3FileService returns a FileService on opts.Bucket. It does not touch
// the network; call EnsureBucket or CheckConnectivity for that.
func NewS3FileService(opts S3Options) (*S3FileService, error) {
	if opts.Bucket == "" {
		return nil, errors.New("s3: bucket is required")
	}
	// With no explicit AK/SK, keep the AWS default credential chain intact.
	loadOptions := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(opts.Region)}
	if opts.AccessKeyID != "" || opts.SecretAccessKey != "" {
		if opts.AccessKeyID == "" || opts.SecretAccessKey == "" {
			return nil, errors.New("s3: access key and secret key must be provided together")
		}
		loadOptions = append(loadOptions, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(opts.AccessKeyID, opts.SecretAccessKey, ""),
		))
	}
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(), loadOptions...)
	if err != nil {
		return nil, fmt.Errorf("s3: load AWS config: %w", err)
	}

	httpClient := &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 10 * time.Minute,
		ExpectContinueTimeout: time.Second,
		IdleConnTimeout:       90 * time.Second,
	}}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.HTTPClient = httpClient
		if opts.Endpoint == "" {
			return // standard AWS S3
		}
		o.BaseEndpoint = aws.String(opts.Endpoint)
		o.UsePathStyle = s3UsePathStyle(opts.Endpoint, opts.ForcePathStyle)
		if !isAWSEndpoint(opts.Endpoint) {
			// S3-compatible services commonly reject the SDK's default
			// trailing checksum negotiation. Only relax this for explicit
			// non-AWS endpoints; standard AWS S3 keeps its default behavior.
			o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
			o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		}
	})
	return &S3FileService{client: client, bucketName: opts.Bucket}, nil
}

var _ interfaces.FileService = (*S3FileService)(nil)

// EnsureBucket creates the bucket if it does not exist yet
// (storage.s3.create_bucket).
func (s *S3FileService) EnsureBucket(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, objectStorageSetupTimeout)
	defer cancel()
	exists, err := s.bucketExists(ctx)
	if err != nil {
		return fmt.Errorf("s3: check bucket %q: %w", s.bucketName, err)
	}
	if exists {
		return nil
	}
	_, err = s.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(s.bucketName)})
	var owned *s3types.BucketAlreadyOwnedByYou
	if err != nil && !errors.As(err, &owned) {
		return fmt.Errorf("s3: create bucket %q: %w", s.bucketName, err)
	}
	return nil
}

// bucketExists checks if the bucket exists
func (s *S3FileService) bucketExists(ctx context.Context) (bool, error) {
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(s.bucketName),
	})
	if err != nil {
		var notFound *s3types.NotFound
		if errors.As(err, &notFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// CheckConnectivity verifies S3 is reachable and the bucket exists. This is
// a read-only probe: it never creates a bucket.
func (s *S3FileService) CheckConnectivity(ctx context.Context) error {
	exists, err := s.bucketExists(ctx)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("bucket %q does not exist", s.bucketName)
	}
	return nil
}

// SaveFile uploads the Source File of one upload to SourceKey.
func (s *S3FileService) SaveFile(ctx context.Context,
	file *multipart.FileHeader, documentID, uploadID uuid.UUID,
) (string, error) {
	key := SourceKey(documentID, uploadID)

	src, err := file.Open()
	if err != nil {
		return "", fmt.Errorf("s3: open upload: %w", err)
	}
	defer func() { _ = src.Close() }()

	ctx, cancel := transferContext(ctx)
	defer cancel()

	// The content type is not trusted from the client: the object is only
	// ever served by Trelis, which sets its own headers.
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucketName),
		Key:           aws.String(key),
		Body:          src,
		ContentLength: aws.Int64(file.Size),
		ContentType:   aws.String("application/octet-stream"),
	})
	if err != nil {
		return "", fmt.Errorf("s3: put source file %s: %w", key, err)
	}
	return key, nil
}

// GetFile opens the object at key.
func (s *S3FileService) GetFile(ctx context.Context, key string) (io.ReadCloser, error) {
	ctx, cancel := transferContext(ctx)
	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(key),
	})
	if err != nil {
		cancel()
		var noKey *s3types.NoSuchKey
		if errors.As(err, &noKey) {
			return nil, interfaces.ErrFileNotFound
		}
		return nil, fmt.Errorf("s3: get %s: %w", key, err)
	}
	return &boundReadCloser{ReadCloser: resp.Body, cancel: cancel}, nil
}

// DeleteFile deletes the object at key. S3 reports success for a missing
// key, so deleting twice is not an error.
func (s *S3FileService) DeleteFile(ctx context.Context, key string) error {
	ctx, cancel := transferContext(ctx)
	defer cancel()
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("s3: delete %s: %w", key, err)
	}
	return nil
}

// ListObjects pages through every object under prefix.
func (s *S3FileService) ListObjects(ctx context.Context, prefix string) ([]interfaces.ObjectInfo, error) {
	ctx, cancel := transferContext(ctx)
	defer cancel()
	pages := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucketName),
		Prefix: aws.String(prefix),
	})
	var objects []interfaces.ObjectInfo
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("s3: list %s: %w", prefix, err)
		}
		for _, object := range page.Contents {
			info := interfaces.ObjectInfo{Key: aws.ToString(object.Key)}
			if object.LastModified != nil {
				info.LastModified = *object.LastModified
			}
			objects = append(objects, info)
		}
	}
	return objects, nil
}

// DeletePrefix deletes every object under prefix in batches of
// deleteBatchSize. An empty prefix is not an error, so a prefix that two
// passes race to remove is removed once and not reported.
func (s *S3FileService) DeletePrefix(ctx context.Context, prefix string) error {
	objects, err := s.ListObjects(ctx, prefix)
	if err != nil {
		return err
	}
	ctx, cancel := transferContext(ctx)
	defer cancel()
	for start := 0; start < len(objects); start += deleteBatchSize {
		batch := objects[start:min(start+deleteBatchSize, len(objects))]
		identifiers := make([]s3types.ObjectIdentifier, 0, len(batch))
		for _, object := range batch {
			identifiers = append(identifiers, s3types.ObjectIdentifier{Key: aws.String(object.Key)})
		}
		out, err := s.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(s.bucketName),
			Delete: &s3types.Delete{Objects: identifiers, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return fmt.Errorf("s3: delete %d objects under %s: %w", len(batch), prefix, err)
		}
		if len(out.Errors) > 0 {
			first := out.Errors[0]
			return fmt.Errorf("s3: delete %d of %d objects under %s: %s failed with %s",
				len(out.Errors), len(batch), prefix, aws.ToString(first.Key), aws.ToString(first.Code))
		}
	}
	return nil
}

// transferContext bounds one transfer unless the caller already did.
func transferContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, objectStorageTransferTimeout)
}

// boundReadCloser releases the transfer context when the body is closed.
type boundReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (r *boundReadCloser) Close() error {
	err := r.ReadCloser.Close()
	r.cancel()
	return err
}
