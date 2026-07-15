package storage

import (
	"context"
	"fmt"
	"io"
	"sort"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3 is an S3-compatible backend (AWS S3, Cloudflare R2, MinIO, ...).
type S3 struct {
	client   *s3.Client
	uploader *manager.Uploader
	bucket   string
}

// S3Options configures an S3 backend.
type S3Options struct {
	Bucket    string
	Endpoint  string
	Region    string
	AccessKey string
	SecretKey string
	PathStyle bool
}

// NewS3 constructs an S3 backend. Uploads stream via multipart, so large
// dumps are never buffered wholesale in memory.
func NewS3(opts S3Options) (*S3, error) {
	if opts.Bucket == "" {
		return nil, fmt.Errorf("s3: bucket is required")
	}
	region := opts.Region
	if region == "" {
		region = "auto"
	}

	s3opts := s3.Options{
		Region:       region,
		Credentials:  credentials.NewStaticCredentialsProvider(opts.AccessKey, opts.SecretKey, ""),
		UsePathStyle: opts.PathStyle,
		// newer SDKs default to trailing CRC checksums, which R2 and many
		// S3-compatible servers reject. only send them when required.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	}
	if opts.Endpoint != "" {
		s3opts.BaseEndpoint = aws.String(opts.Endpoint)
	}
	client := s3.New(s3opts)

	return &S3{
		client:   client,
		uploader: manager.NewUploader(client),
		bucket:   opts.Bucket,
	}, nil
}

// Put streams r to key via multipart upload.
func (s *S3) Put(ctx context.Context, key string, r io.Reader) error {
	_, err := s.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   r,
	})
	if err != nil {
		return fmt.Errorf("s3 put %s: %w", key, err)
	}
	return nil
}

// Get opens key for reading. The caller must Close the returned reader.
func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("s3 get %s: %w", key, err)
	}
	return out.Body, nil
}

// List returns keys under prefix, sorted.
func (s *S3) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("s3 list %s: %w", prefix, err)
		}
		for _, obj := range page.Contents {
			if obj.Key != nil {
				keys = append(keys, *obj.Key)
			}
		}
	}
	sort.Strings(keys)
	return keys, nil
}

// Delete removes key. S3 DeleteObject is already idempotent.
func (s *S3) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("s3 delete %s: %w", key, err)
	}
	return nil
}

// Stat returns the size in bytes of key.
func (s *S3) Stat(ctx context.Context, key string) (int64, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return 0, fmt.Errorf("s3 stat %s: %w", key, err)
	}
	if out.ContentLength == nil {
		return 0, nil
	}
	return *out.ContentLength, nil
}
