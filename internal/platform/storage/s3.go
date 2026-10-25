package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"koro/internal/platform/clock"
)

// DefaultTimeout bounds one S3 request, body transfer included.
const DefaultTimeout = 30 * time.Second

// Config configures NewS3. cmd/koro maps config.Storage onto it.
type Config struct {
	// Endpoint is the S3 API the server talks to ("http://127.0.0.1:8333").
	Endpoint string
	// PresignEndpoint is the S3 API browsers reach; presigned URLs are
	// signed for its host. Empty means Endpoint.
	PresignEndpoint string
	Region          string // "us-east-1" for SeaweedFS
	AccessKey       string
	SecretKey       string
	// PathStyle addresses buckets as /<bucket>/<key> (SeaweedFS and most
	// S3-compatible providers); false uses virtual-hosted style.
	PathStyle bool
	Timeout   time.Duration // 0 → DefaultTimeout
	Clock     clock.Clock   // nil → clock.Real()
}

// S3 implements Objects over aws-sdk-go-v2.
type S3 struct {
	api     *s3.Client
	presign *s3.PresignClient
	clock   clock.Clock
}

var _ Objects = (*S3)(nil)

// NewS3 builds the client. It performs no network call.
func NewS3(cfg Config) (*S3, error) {
	if cfg.Endpoint == "" || cfg.Region == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, fmt.Errorf("%w: endpoint, region and credentials are required", ErrInvalidInput)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.Real()
	}
	if cfg.PresignEndpoint == "" {
		cfg.PresignEndpoint = cfg.Endpoint
	}
	awsCfg := aws.Config{
		Region:      cfg.Region,
		Credentials: credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		HTTPClient:  awshttp.NewBuildableClient().WithTimeout(cfg.Timeout),
		// R-002 §3i, 05 §14.7: no default CRC32 checksum header or trailer
		// on requests, no mandatory checksum validation on responses.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		RetryMaxAttempts:           3,
	}
	client := func(endpoint string) *s3.Client {
		return s3.NewFromConfig(awsCfg, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(endpoint)
			o.UsePathStyle = cfg.PathStyle
		})
	}
	api := client(cfg.Endpoint)
	pre := api
	if cfg.PresignEndpoint != cfg.Endpoint {
		pre = client(cfg.PresignEndpoint)
	}
	return &S3{api: api, presign: s3.NewPresignClient(pre), clock: cfg.Clock}, nil
}

// PutObject implements Objects.
func (s *S3) PutObject(ctx context.Context, in PutInput) error {
	if err := in.validate(); err != nil {
		return err
	}
	_, err := s.api.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(in.Bucket), Key: aws.String(in.Key), Body: in.Body,
		ContentLength: aws.Int64(in.Size), ContentType: aws.String(in.ContentType),
		ContentDisposition: optional(in.ContentDisposition),
	})
	return wrap("put object", err)
}

// PresignPut implements Objects.
func (s *S3) PresignPut(ctx context.Context, in PresignPutInput) (Presigned, error) {
	if err := in.validate(); err != nil {
		return Presigned{}, err
	}
	now := s.clock.Now()
	req, err := s.presign.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(in.Bucket), Key: aws.String(in.Key),
		ContentType: aws.String(in.ContentType), ContentLength: aws.Int64(in.ContentLength),
	}, s3.WithPresignExpires(in.TTL))
	if err != nil {
		return Presigned{}, wrap("presign put", err)
	}
	return Presigned{URL: req.URL, Method: req.Method, Headers: clientHeaders(req.SignedHeader), ExpiresAt: now.Add(in.TTL)}, nil
}

// PresignGet implements Objects.
func (s *S3) PresignGet(ctx context.Context, in PresignGetInput) (Presigned, error) {
	if err := in.validate(); err != nil {
		return Presigned{}, err
	}
	now := s.clock.Now()
	req, err := s.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(in.Bucket), Key: aws.String(in.Key),
		ResponseContentDisposition: aws.String(in.ResponseContentDisposition),
		ResponseContentType:        aws.String(in.ResponseContentType),
		ResponseCacheControl:       aws.String("private, no-store"),
	}, s3.WithPresignExpires(in.TTL))
	if err != nil {
		return Presigned{}, wrap("presign get", err)
	}
	return Presigned{URL: req.URL, Method: req.Method, Headers: clientHeaders(req.SignedHeader), ExpiresAt: now.Add(in.TTL)}, nil
}

// Stat implements Objects.
func (s *S3) Stat(ctx context.Context, bucket, key string) (ObjectInfo, error) {
	out, err := s.api.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return ObjectInfo{}, wrap("stat object", err)
	}
	return ObjectInfo{Size: aws.ToInt64(out.ContentLength), ContentType: aws.ToString(out.ContentType),
		CacheControl: aws.ToString(out.CacheControl)}, nil
}

// Open implements Objects.
func (s *S3) Open(ctx context.Context, bucket, key string) (io.ReadCloser, ObjectInfo, error) {
	out, err := s.api.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return nil, ObjectInfo{}, wrap("open object", err)
	}
	return out.Body, ObjectInfo{Size: aws.ToInt64(out.ContentLength), ContentType: aws.ToString(out.ContentType),
		CacheControl: aws.ToString(out.CacheControl)}, nil
}

// Copy implements Objects.
func (s *S3) Copy(ctx context.Context, in CopyInput) error {
	if err := in.validate(); err != nil {
		return err
	}
	src := (&url.URL{Path: in.SrcBucket + "/" + in.SrcKey}).EscapedPath()
	_, err := s.api.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String(in.DstBucket), Key: aws.String(in.DstKey), CopySource: aws.String(src),
		MetadataDirective: types.MetadataDirectiveReplace,
		ContentType:       aws.String(in.ContentType), ContentDisposition: optional(in.ContentDisposition),
		CacheControl: optional(in.CacheControl),
	})
	return wrap("copy object", err)
}

// Delete implements Objects.
func (s *S3) Delete(ctx context.Context, bucket, key string) error {
	_, err := s.api.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err = wrap("delete object", err); errors.Is(err, ErrNotFound) {
		return nil // S3 answers 204 for a missing key; some servers answer 404
	}
	return err
}

// CreateBucket creates a private bucket (tests and local tooling only; the
// production buckets are created by operations with lifecycle rules).
func (s *S3) CreateBucket(ctx context.Context, bucket string) error {
	_, err := s.api.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
	var owned *types.BucketAlreadyOwnedByYou
	if errors.As(err, &owned) {
		return nil
	}
	return wrap("create bucket", err)
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return aws.String(s)
}

// wrap maps a missing object onto ErrNotFound and names the operation. The
// SDK error carries no credentials (the request signature is not part of it).
func wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "NoSuchBucket":
			return fmt.Errorf("storage: %s: %w (%s)", op, ErrNotFound, apiErr.ErrorCode())
		}
	}
	var re *awshttp.ResponseError
	if errors.As(err, &re) && re.HTTPStatusCode() == http.StatusNotFound {
		return fmt.Errorf("storage: %s: %w", op, ErrNotFound)
	}
	return fmt.Errorf("storage: %s: %w", op, err)
}
