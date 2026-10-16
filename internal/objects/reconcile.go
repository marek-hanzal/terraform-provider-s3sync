package objects

import (
	"context"
	"crypto/md5" // Explicit ETag compatibility only; SHA-256 remains the content contract.
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

type Request struct {
	Bucket      string
	Prefix      string
	Files       map[string]File
	Previous    map[string]Record
	Concurrency int
}

type Result struct {
	Inventory map[string]Record
	Uploaded  int
	Skipped   int
}

type preparedFile struct {
	File
	size int64
	md5  string
	info os.FileInfo
}

func (client *Client) Reconcile(ctx context.Context, request Request) (Result, error) {
	result := Result{Inventory: copyInventory(request.Previous)}
	if err := ValidateDestination(request.Bucket, request.Prefix, false); err != nil {
		return result, err
	}
	if err := ValidateFiles(request.Files); err != nil {
		return result, err
	}
	if err := validateInventory(request.Previous); err != nil {
		return result, err
	}
	if request.Concurrency < 0 {
		return result, fmt.Errorf("concurrency must be positive")
	}
	if request.Concurrency == 0 {
		request.Concurrency = 4
	}
	prepared, err := client.prepare(ctx, request.Files)
	if err != nil {
		return result, err
	}
	remote, err := client.List(ctx, request.Bucket, request.Prefix)
	if err != nil {
		return result, err
	}

	// An internal failure stops admission, but does not cancel work already
	// admitted. Every successful acknowledgement must reach the partial state so
	// a subsequent apply can resume without replaying acknowledged uploads.
	jobs := make(chan string)
	failed := make(chan struct{})
	var stop sync.Once
	var workers sync.WaitGroup
	var mutation sync.Mutex
	var firstError error
	for range request.Concurrency {
		workers.Go(func() {
			for key := range jobs {
				file := prepared[key]
				record, uploaded, err := client.reconcileFile(ctx, request, key, file, remote[key])
				mutation.Lock()
				if err != nil {
					if firstError == nil {
						firstError = err
					}
					stop.Do(func() { close(failed) })
				} else {
					result.Inventory[key] = record
					if uploaded {
						result.Uploaded++
					} else {
						result.Skipped++
					}
				}
				mutation.Unlock()
			}
		})
	}
admit:
	for _, key := range sortedKeys(prepared) {
		select {
		case <-failed:
			break admit
		case <-ctx.Done():
			break admit
		default:
		}
		select {
		case jobs <- key:
		case <-failed:
			break admit
		case <-ctx.Done():
			break admit
		}
	}
	close(jobs)
	workers.Wait()
	if firstError != nil {
		return result, firstError
	}
	if err := ctx.Err(); err != nil {
		return result, safeError("sync", "", err)
	}
	return result, nil
}

func (client *Client) reconcileFile(ctx context.Context, request Request, key string, file preparedFile, remote Remote) (Record, bool, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, false, safeError("sync", key, err)
	}
	previous, acknowledged := request.Previous[key]
	if acknowledged && previous.SHA256 == file.SHA256 && previous.ContentType == file.ContentType &&
		previous.Size == file.size && previous.ETag != "" && previous.ETag == remote.ETag && remote.Size == file.size {
		// ListObjectsV2 has no content type. Acknowledged metadata is trusted
		// while body identity and size remain unchanged; external metadata-only
		// mutations are an explicit bulk-refresh limitation.
		return previous, false, nil
	}
	if client.matchesHash(remote.ETag, file) && remote.Size == file.size {
		head, err := client.api.HeadObject(ctx, &s3.HeadObjectInput{
			Bucket: aws.String(request.Bucket), Key: aws.String(request.Prefix + key),
		})
		if err != nil && !missing(err) {
			return Record{}, false, safeError("inspect", key, err)
		}
		if err == nil && etag(aws.ToString(head.ETag)) == remote.ETag && aws.ToInt64(head.ContentLength) == file.size &&
			aws.ToString(head.ContentType) == file.ContentType {
			return Record{SHA256: file.SHA256, ETag: remote.ETag, ContentType: file.ContentType, Size: file.size}, false, nil
		}
	}
	record, err := client.upload(ctx, request.Bucket, request.Prefix, key, file)
	return record, err == nil, err
}

func (client *Client) matchesHash(remoteETag string, file preparedFile) bool {
	switch client.etagHash {
	case "sha256":
		return strings.EqualFold(remoteETag, file.SHA256)
	case "md5":
		return strings.EqualFold(remoteETag, file.md5)
	default:
		return false
	}
}

func (client *Client) prepare(ctx context.Context, files map[string]File) (map[string]preparedFile, error) {
	prepared := make(map[string]preparedFile, len(files))
	for _, key := range sortedKeys(files) {
		if err := ctx.Err(); err != nil {
			return nil, safeError("verify source", key, err)
		}
		file := files[key]
		if !filepath.IsAbs(file.Source) {
			if client.sourceDirectory == "" {
				return nil, fmt.Errorf("verify source object %q failed (relative source requires source_directory)", key)
			}
			// Resolve a copy for I/O; configured paths and the resource digest stay relative.
			file.Source = filepath.Join(client.sourceDirectory, filepath.FromSlash(file.Source))
		}
		source, info, err := openSource(file.Source)
		if err != nil {
			return nil, fmt.Errorf("verify source object %q failed (%s)", key, err)
		}
		checksum := sha256.New()
		md5Checksum := md5.New()
		var writer io.Writer = checksum
		if client.etagHash == "md5" {
			writer = io.MultiWriter(checksum, md5Checksum)
		}
		size, readError := io.Copy(writer, contextReader{ctx: ctx, reader: source})
		closeError := source.Close()
		if readError != nil {
			if ctx.Err() != nil {
				return nil, safeError("verify source", key, ctx.Err())
			}
			return nil, fmt.Errorf("verify source object %q failed (read_error)", key)
		}
		if closeError != nil {
			return nil, fmt.Errorf("verify source object %q failed (close_error)", key)
		}
		file.SHA256 = strings.ToLower(file.SHA256)
		file.ContentType = contentType(file.ContentType)
		if hex.EncodeToString(checksum.Sum(nil)) != file.SHA256 || size != info.Size() {
			return nil, fmt.Errorf("verify source object %q failed (sha256_mismatch)", key)
		}
		prepared[key] = preparedFile{File: file, size: size, md5: hex.EncodeToString(md5Checksum.Sum(nil)), info: info}
	}
	return prepared, nil
}

func (client *Client) upload(ctx context.Context, bucket, prefix, key string, file preparedFile) (Record, error) {
	source, info, err := openSource(file.Source)
	if err != nil {
		return Record{}, fmt.Errorf("open source object %q failed (%s)", key, err)
	}
	defer source.Close()
	if !os.SameFile(info, file.info) || info.Size() != file.size || !info.ModTime().Equal(file.info.ModTime()) {
		return Record{}, fmt.Errorf("upload object %q failed (source_changed)", key)
	}
	checksum, _ := hex.DecodeString(file.SHA256) // Manifest validation already established the encoding.
	response, err := client.api.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(prefix + key), Body: source,
		ContentLength: aws.Int64(file.size), ContentType: aws.String(file.ContentType),
		ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(checksum)),
	})
	if err != nil {
		return Record{}, safeError("upload", key, err)
	}
	acknowledgedETag := etag(aws.ToString(response.ETag))
	if acknowledgedETag == "" {
		return Record{}, fmt.Errorf("upload object %q failed (missing_etag_acknowledgement)", key)
	}
	return Record{SHA256: file.SHA256, ETag: acknowledgedETag, ContentType: file.ContentType, Size: file.size}, nil
}

func openSource(path string) (*os.File, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, errors.New("stat_error")
	}
	if !info.Mode().IsRegular() {
		return nil, nil, errors.New("nonregular_source")
	}
	source, err := os.Open(path)
	if err != nil {
		return nil, nil, errors.New("open_error")
	}
	opened, err := source.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		source.Close()
		return nil, nil, errors.New("source_changed")
	}
	return source, opened, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader contextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}

func missing(err error) bool {
	var apiError smithy.APIError
	var responseError *smithyhttp.ResponseError
	return errors.As(err, &apiError) && (apiError.ErrorCode() == "NotFound" || apiError.ErrorCode() == "NoSuchKey") ||
		errors.As(err, &responseError) && responseError.HTTPStatusCode() == 404
}

func validateInventory(inventory map[string]Record) error {
	return ValidateKeys(sortedKeys(inventory))
}

func copyInventory(previous map[string]Record) map[string]Record {
	inventory := make(map[string]Record, len(previous))
	for key, record := range previous {
		inventory[key] = record
	}
	return inventory
}
