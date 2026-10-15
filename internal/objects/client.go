package objects

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

type Config struct {
	Endpoint       string
	Region         string
	AccessKey      string
	SecretKey      string
	ETagHash       string
	MaxAttempts    int
	RequestTimeout time.Duration
}

type s3API interface {
	ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
}

type Client struct {
	api      s3API
	etagHash string
}

func New(ctx context.Context, config Config) (*Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, safeError("configure", "", err)
	}
	if config.Endpoint != "" {
		endpoint, err := url.Parse(config.Endpoint)
		if err != nil || endpoint.Host == "" || (endpoint.Scheme != "https" && endpoint.Scheme != "http") ||
			endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" ||
			(endpoint.Path != "" && endpoint.Path != "/") {
			return nil, fmt.Errorf("endpoint must be an HTTP(S) origin without credentials, path, query or fragment")
		}
	}
	if config.AccessKey == "" || config.SecretKey == "" {
		return nil, fmt.Errorf("access key and secret key are required")
	}
	if config.Region == "" {
		config.Region = "us-east-1"
	}
	if config.ETagHash == "" {
		config.ETagHash = "opaque"
	}
	if config.ETagHash != "opaque" && config.ETagHash != "md5" && config.ETagHash != "sha256" {
		return nil, fmt.Errorf("etag_hash must be opaque, md5 or sha256")
	}
	if config.MaxAttempts < 0 || config.RequestTimeout < 0 {
		return nil, fmt.Errorf("max attempts and request timeout must be positive")
	}
	if config.MaxAttempts == 0 {
		config.MaxAttempts = 5
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = 120 * time.Second
	}
	api := s3.NewFromConfig(aws.Config{
		Region:      config.Region,
		Credentials: credentials.NewStaticCredentialsProvider(config.AccessKey, config.SecretKey, ""),
		HTTPClient:  awshttp.NewBuildableClient().WithTimeout(config.RequestTimeout),
		Retryer: func() aws.Retryer {
			return retry.NewStandard(func(options *retry.StandardOptions) { options.MaxAttempts = config.MaxAttempts })
		},
	}, func(options *s3.Options) {
		if config.Endpoint != "" {
			options.BaseEndpoint = aws.String(config.Endpoint)
		}
		options.UsePathStyle = true
		options.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		options.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
	return &Client{api: api, etagHash: config.ETagHash}, nil
}

func (client *Client) List(ctx context.Context, bucket, prefix string) (map[string]Remote, error) {
	if err := ValidateDestination(bucket, prefix, false); err != nil {
		return nil, err
	}
	remote := make(map[string]Remote)
	request := &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket), Prefix: aws.String(prefix), MaxKeys: aws.Int32(1000),
	}
	seenTokens := make(map[string]bool)
	for {
		page, err := client.api.ListObjectsV2(ctx, request)
		if err != nil {
			return nil, safeError("list", "", err)
		}
		for _, object := range page.Contents {
			key := aws.ToString(object.Key)
			if !strings.HasPrefix(key, prefix) {
				return nil, fmt.Errorf("list returned an object outside the requested prefix")
			}
			key = strings.TrimPrefix(key, prefix)
			// Directory markers have no file identity and must never become owned
			// through a listing-based prune or Terraform resource destruction.
			if key == "" || strings.HasSuffix(key, "/") {
				continue
			}
			if err := validKey(key); err != nil {
				return nil, fmt.Errorf("list returned an unsupported relative object key %q", key)
			}
			remote[key] = Remote{ETag: etag(aws.ToString(object.ETag)), Size: aws.ToInt64(object.Size)}
		}
		if !aws.ToBool(page.IsTruncated) {
			break
		}
		token := aws.ToString(page.NextContinuationToken)
		if token == "" || seenTokens[token] {
			return nil, fmt.Errorf("list failed (invalid_pagination)")
		}
		seenTokens[token] = true
		request.ContinuationToken = aws.String(token)
	}
	return remote, nil
}

func etag(value string) string {
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		return value[1 : len(value)-1]
	}
	return value
}

// SDK errors may embed the signed endpoint, credentials, request headers or a
// server's arbitrary response text. Only operation identity and safe codes cross
// the Terraform diagnostic boundary.
func safeError(operation, key string, err error) error {
	code := "transport_error"
	var apiError smithy.APIError
	var responseError *smithyhttp.ResponseError
	var networkError net.Error
	switch {
	case errors.Is(err, context.Canceled):
		code = "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		code = "deadline_exceeded"
	case errors.As(err, &apiError):
		code = "api_error"
		switch apiError.ErrorCode() {
		case "AccessDenied", "NoSuchBucket", "NoSuchKey", "NotFound", "SlowDown", "RequestTimeout", "InternalError",
			"ServiceUnavailable", "BadDigest", "InvalidRequest", "InvalidArgument", "InvalidAccessKeyId", "SignatureDoesNotMatch":
			code = apiError.ErrorCode()
		}
	case errors.As(err, &responseError):
		code = fmt.Sprintf("http_%d", responseError.HTTPStatusCode())
	case errors.As(err, &networkError) && networkError.Timeout():
		code = "request_timeout"
	}
	if key == "" {
		return fmt.Errorf("%s failed (%s)", operation, code)
	}
	return fmt.Errorf("%s object %q failed (%s)", operation, key, code)
}
