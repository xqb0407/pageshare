package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"pageshare/internal/config"
	"pageshare/internal/site"
)

// S3 面向标准 S3 兼容 API（AWS S3 / Cloudflare R2 / MinIO），桶保持私有。
type S3 struct {
	client  *s3.Client
	presign *s3.PresignClient
	bucket  string
	prefix  string
}

// NewS3 按配置构造客户端。endpoint 留空走 AWS 默认解析。
func NewS3(ctx context.Context, sc config.Storage) (*S3, error) {
	if sc.Bucket == "" {
		return nil, errors.New("storage.bucket 不能为空")
	}
	var opts []func(*awsconfig.LoadOptions) error
	if sc.Region != "" {
		opts = append(opts, awsconfig.WithRegion(sc.Region))
	}
	if sc.AccessKeyID != "" || sc.SecretAccessKey != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
				return aws.Credentials{
					AccessKeyID:     sc.AccessKeyID,
					SecretAccessKey: sc.SecretAccessKey,
				}, nil
			})))
	}
	if sc.Endpoint != "" {
		opts = append(opts, awsconfig.WithBaseEndpoint(sc.Endpoint))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("加载 AWS 配置: %w", err)
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if sc.Endpoint != "" {
			o.BaseEndpoint = aws.String(sc.Endpoint)
			// R2 / MinIO 走 path-style；AWS 保持默认 virtual-host。
			if strings.Contains(sc.Endpoint, "r2.cloudflarestorage.com") ||
				strings.Contains(sc.Endpoint, "localhost") ||
				strings.Contains(sc.Endpoint, "127.0.0.1") {
				o.UsePathStyle = true
			}
		}
	})
	return &S3{
		client:  client,
		presign: s3.NewPresignClient(client),
		bucket:  sc.Bucket,
		prefix:  sc.Prefix,
	}, nil
}

func (s *S3) key(k string) string { return s.prefix + k }

func (s *S3) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(s.key(key)),
		Body:          r,
		ContentLength: aws.Int64(size),
		ContentType:   aws.String(contentType),
		CacheControl:  aws.String(site.CacheControlFor(key, contentType)),
	})
	return err
}

func (s *S3) Get(ctx context.Context, key string) (*Object, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key(key)),
	})
	if err != nil {
		if isNoSuchKey(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	ct := ""
	if out.ContentType != nil {
		ct = *out.ContentType
	}
	return &Object{Body: out.Body, ContentType: ct, Size: deref64(out.ContentLength)}, nil
}

func (s *S3) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	prefix = s.key(prefix)
	n := 0
	var batch []s3types.ObjectIdentifier
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		out, err := s.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(s.bucket),
			Delete: &s3types.Delete{Objects: batch, Quiet: aws.Bool(true)},
		})
		n += len(batch) - len(out.Errors)
		batch = batch[:0]
		return err
	}
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return n, err
		}
		for _, obj := range page.Contents {
			batch = append(batch, s3types.ObjectIdentifier{Key: obj.Key})
			if len(batch) == 1000 {
				if err := flush(); err != nil {
					return n, err
				}
			}
		}
	}
	return n, flush()
}

func (s *S3) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	req, err := s.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key(key)),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", err
	}
	return req.URL, nil
}

func isNoSuchKey(err error) bool {
	var nf *s3types.NoSuchKey
	if errors.As(err, &nf) {
		return true
	}
	return strings.Contains(err.Error(), "StatusCode: 404")
}

func deref64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

var _ Storage = (*S3)(nil)
var _ Storage = (*Mem)(nil)
