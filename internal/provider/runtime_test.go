package provider_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// This opt-in test targets an independently implemented S3 runtime, such as
// SeaweedFS. The httptest fixture separately proves request counts and failures.
func TestTerraformIndependentS3Runtime(t *testing.T) {
	endpoint := os.Getenv("S3SYNC_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("set S3SYNC_TEST_ENDPOINT, S3SYNC_TEST_ACCESS_KEY and S3SYNC_TEST_SECRET_KEY for independent S3 runtime evidence")
	}
	accessKey, secretKey := os.Getenv("S3SYNC_TEST_ACCESS_KEY"), os.Getenv("S3SYNC_TEST_SECRET_KEY")
	if accessKey == "" || secretKey == "" {
		t.Fatal("independent S3 runtime requires explicit test credentials")
	}
	region := os.Getenv("S3SYNC_TEST_REGION")
	if region == "" {
		region = "us-east-1"
	}
	client := s3.NewFromConfig(aws.Config{Region: region, Credentials: credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")}, func(options *s3.Options) { options.BaseEndpoint = aws.String(endpoint); options.UsePathStyle = true })
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	bucket := fmt.Sprintf("s3sync-prototype-%d", time.Now().UnixNano())
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("create independent runtime test bucket: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		pages := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
		for pages.HasMorePages() {
			page, err := pages.NextPage(cleanupCtx)
			if err != nil {
				t.Errorf("list cleanup bucket: %v", err)
				return
			}
			for _, object := range page.Contents {
				if _, err := client.DeleteObject(cleanupCtx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: object.Key}); err != nil {
					t.Errorf("delete cleanup object: %v", err)
				}
			}
		}
		if _, err := client.DeleteBucket(cleanupCtx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)}); err != nil {
			t.Errorf("delete cleanup bucket: %v", err)
		}
	})
	put := func(key, body string) {
		t.Helper()
		if _, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader([]byte(body)), ContentType: aws.String("application/octet-stream")}); err != nil {
			t.Fatalf("seed independent runtime: %v", err)
		}
	}
	assert := func(key, want string) {
		t.Helper()
		output, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
		if err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
		body, err := io.ReadAll(output.Body)
		_ = output.Body.Close()
		if err != nil || string(body) != want {
			t.Fatalf("%s body=%q err=%v want=%q", key, body, err, want)
		}
	}

	tf := newTerraform(t, buildProvider(t), nil)
	tf.endpoint, tf.accessKey, tf.secretKey, tf.region, tf.bucket = endpoint, accessKey, secretKey, region, bucket
	// Opaque is the portable default: an SDK-specific ETag must never be guessed
	// to be a SHA256, MD5 or multipart checksum.
	tf.etagHash = ""
	tf.file("first.bin", "first", "application/octet-stream")
	tf.file("second.bin", "other", "application/octet-stream")
	if os.Getenv("S3SYNC_TEST_BULK") == "1" {
		body := string(bytes.Repeat([]byte("0123456789abcdef"), 20*1024))
		for index := 0; index < 1658; index++ {
			tf.file(fmt.Sprintf("bulk/%04d.bin", index), body, "application/octet-stream")
		}
		t.Log("bulk workload: 1,660 asset files, 543,293,450 bytes (518.125 MiB), plus one HTML document")
	}
	tf.documents = true
	tf.prune = true
	put("assets/obsolete.bin", "obsolete")
	put("unrelated/preserve.bin", "preserve")
	tf.writeConfig()
	tf.run(0, "init", "-input=false")
	tf.apply()
	assert("assets/first.bin", "first")
	assert("site/index.html", "index-v1")
	assert("unrelated/preserve.bin", "preserve")
	tf.plan(0, "unchanged.plan")
	tf.run(0, "apply", "-input=false", "-auto-approve", "unchanged.plan")
	tf.file("first.bin", "FIRST", "application/octet-stream")
	tf.writeConfig()
	tf.apply()
	assert("assets/first.bin", "FIRST")
	put("assets/second.bin", "BROKE")
	tf.plan(2, "drift.plan")
	tf.run(0, "apply", "-input=false", "-auto-approve", "drift.plan")
	assert("assets/second.bin", "other")
	tf.run(0, "destroy", "-input=false", "-auto-approve")
	assert("assets/first.bin", "FIRST")
	assert("site/index.html", "index-v1")
}
