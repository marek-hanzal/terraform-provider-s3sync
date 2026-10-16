package objects

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"mime"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

type File struct {
	Source      string
	SHA256      string
	ContentType string
}

type Record struct {
	SHA256      string `json:"sha256"`
	ETag        string `json:"etag"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
}

type Remote struct {
	ETag string
	Size int64
}

var bucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

func ValidateDestination(bucket, prefix string, requirePrefix bool) error {
	if !bucketPattern.MatchString(bucket) || strings.Contains(bucket, "..") {
		return fmt.Errorf("bucket must be a DNS-style S3 bucket name")
	}
	if prefix == "" {
		if requirePrefix {
			return fmt.Errorf("pruning requires a nonempty exclusive prefix ending in /")
		}
		return nil
	}
	if !strings.HasSuffix(prefix, "/") || validKey(strings.TrimSuffix(prefix, "/")) != nil {
		return fmt.Errorf("prefix must be a relative path ending in / with no empty or dot segments")
	}
	return nil
}

// ValidateFiles checks the manifest independently of source availability. Terraform
// planning may run before a preceding resource has generated those local files.
func ValidateFiles(files map[string]File) error {
	if err := ValidateKeys(sortedKeys(files)); err != nil {
		return err
	}
	for _, key := range sortedKeys(files) {
		file := files[key]
		if hasControl(file.Source) || (!filepath.IsAbs(file.Source) && (validKey(file.Source) != nil || strings.Contains(file.Source, ":") || filepath.VolumeName(file.Source) != "")) {
			return fmt.Errorf("object %q source must be absolute or an unambiguous relative slash-separated path", key)
		}
		checksum, err := hex.DecodeString(file.SHA256)
		if err != nil || len(checksum) != sha256.Size || file.SHA256 != strings.ToLower(file.SHA256) {
			return fmt.Errorf("object %q sha256 must contain exactly 64 lowercase hexadecimal characters", key)
		}
		if hasControl(file.ContentType) {
			return fmt.Errorf("object %q content type contains control characters", key)
		}
		if _, _, err := mime.ParseMediaType(contentType(file.ContentType)); err != nil {
			return fmt.Errorf("object %q content type is invalid", key)
		}
	}
	return nil
}

func ValidateKeys(keys []string) error {
	for _, key := range keys {
		if validKey(key) != nil {
			return fmt.Errorf("invalid relative object key %q", key)
		}
	}
	return nil
}

// Digest frames each value so distinct manifests cannot share an ambiguous
// concatenation. Configured source paths are desired input; provider-local roots
// stay outside resource identity so relocating an identical manifest is a no-op.
func Digest(files map[string]File) string {
	digest := sha256.New()
	for _, key := range sortedKeys(files) {
		file := files[key]
		for _, value := range []string{key, file.Source, strings.ToLower(file.SHA256), contentType(file.ContentType)} {
			writeDigestValue(digest, value)
		}
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func writeDigestValue(digest hash.Hash, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = digest.Write(length[:])
	_, _ = digest.Write([]byte(value))
}

func validKey(key string) error {
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "\\") || hasControl(key) {
		return fmt.Errorf("invalid key")
	}
	for _, segment := range strings.Split(key, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("invalid key segment")
		}
	}
	return nil
}

func hasControl(value string) bool {
	return strings.ContainsFunc(value, unicode.IsControl)
}

func contentType(value string) string {
	if value == "" {
		return "application/octet-stream"
	}
	return value
}

func sortedKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
