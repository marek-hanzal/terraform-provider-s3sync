// Package teststore provides the deliberately small S3 surface supported by Bunny
// Storage. It is test support, never an alternate production transport.
package teststore

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type Object struct {
	Body        []byte
	ContentType string
	ETag        string
}

type Stats struct {
	List        int
	Head        int
	Put         int
	Delete      int
	PutByKey    map[string]int
	DeleteByKey map[string]int
	Bodies      map[string][][]byte
}

// Failure models retryable responses or a lost connection after consuming the
// request body. Consuming the body makes stream replay a meaningful assertion.
type Failure struct {
	Remaining  int
	Status     int
	Disconnect bool
}

type Store struct {
	Server   *httptest.Server
	mu       sync.Mutex
	objects  map[string]Object
	failures map[string]Failure
	blocks   map[string]<-chan struct{}
	hash     string
	pageSize int
	stats    Stats
}

func New(t testing.TB) *Store {
	t.Helper()
	s := &Store{objects: map[string]Object{}, failures: map[string]Failure{}, blocks: map[string]<-chan struct{}{}, hash: "sha256", pageSize: 1000}
	s.ResetStats()
	s.Server = httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	t.Cleanup(s.Server.Close)
	return s
}

func (s *Store) URL() string             { return s.Server.URL }
func (s *Store) SetETagHash(mode string) { s.mu.Lock(); defer s.mu.Unlock(); s.hash = mode }
func (s *Store) SetPageSize(n int)       { s.mu.Lock(); defer s.mu.Unlock(); s.pageSize = n }
func (s *Store) Fail(key string, failure Failure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures[key] = failure
}
func (s *Store) Block(key string, release <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blocks[key] = release
}
func (s *Store) Seed(bucket, key string, body []byte, contentType string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[bucket+"/"+key] = Object{Body: append([]byte(nil), body...), ContentType: contentType, ETag: etag(s.hash, body)}
}
func (s *Store) Remove(bucket, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, bucket+"/"+key)
}
func (s *Store) Object(bucket, key string) (Object, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	object, ok := s.objects[bucket+"/"+key]
	object.Body = append([]byte(nil), object.Body...)
	return object, ok
}
func (s *Store) ResetStats() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats = Stats{PutByKey: map[string]int{}, DeleteByKey: map[string]int{}, Bodies: map[string][][]byte{}}
}
func (s *Store) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.stats
	v.PutByKey = map[string]int{}
	v.DeleteByKey = map[string]int{}
	v.Bodies = map[string][][]byte{}
	for k, n := range s.stats.PutByKey {
		v.PutByKey[k] = n
	}
	for k, n := range s.stats.DeleteByKey {
		v.DeleteByKey[k] = n
	}
	for k, items := range s.stats.Bodies {
		for _, body := range items {
			v.Bodies[k] = append(v.Bodies[k], append([]byte(nil), body...))
		}
	}
	return v
}
func etag(mode string, body []byte) string {
	if mode == "md5" {
		sum := md5.Sum(body)
		return hex.EncodeToString(sum[:])
	}
	sum := sha256.Sum256(body)
	value := hex.EncodeToString(sum[:])
	if mode == "opaque" {
		return "opaque-" + value
	}
	return value
}

func (s *Store) serveHTTP(w http.ResponseWriter, r *http.Request) {
	for name := range r.Header {
		lower := strings.ToLower(name)
		if lower == "x-amz-acl" || lower == "x-amz-tagging" || strings.HasPrefix(lower, "x-amz-meta-") {
			s.failHTTP(w, 400, "UnsupportedOperation", "Bunny does not support object metadata, tagging or ACLs")
			return
		}
	}
	if r.URL.Query().Has("delete") || r.URL.Query().Has("tagging") || r.URL.Query().Has("acl") {
		s.failHTTP(w, 400, "UnsupportedOperation", "Unsupported Bunny operation")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")
	if bucket == "" {
		s.failHTTP(w, 400, "InvalidBucketName", "Missing bucket")
		return
	}
	if r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2" {
		s.list(w, r, bucket)
		return
	}
	if key == "" {
		s.failHTTP(w, 400, "InvalidRequest", "Expected object key")
		return
	}
	full := bucket + "/" + key
	switch r.Method {
	case http.MethodPut:
		s.put(w, r, full, key)
	case http.MethodHead, http.MethodGet:
		s.mu.Lock()
		object, ok := s.objects[full]
		if r.Method == http.MethodHead {
			s.stats.Head++
		}
		s.mu.Unlock()
		if !ok {
			s.failHTTP(w, 404, "NoSuchKey", "Missing object")
			return
		}
		w.Header().Set("ETag", `"`+object.ETag+`"`)
		w.Header().Set("Content-Type", object.ContentType)
		w.Header().Set("Content-Length", strconv.Itoa(len(object.Body)))
		w.Header().Set("Last-Modified", time.Unix(1700000000, 0).UTC().Format(http.TimeFormat))
		if r.Method == http.MethodGet {
			_, _ = w.Write(object.Body)
		}
	case http.MethodDelete:
		s.mu.Lock()
		s.stats.Delete++
		s.stats.DeleteByKey[key]++
		delete(s.objects, full)
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		s.failHTTP(w, 400, "UnsupportedOperation", "Unsupported S3 operation")
	}
}
func (s *Store) put(w http.ResponseWriter, r *http.Request, full, key string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.failHTTP(w, 400, "InvalidRequest", "Invalid body")
		return
	}
	s.mu.Lock()
	s.stats.Put++
	s.stats.PutByKey[key]++
	s.stats.Bodies[key] = append(s.stats.Bodies[key], body)
	failure := s.failures[key]
	shouldFail := failure.Remaining > 0
	if shouldFail {
		failure.Remaining--
		s.failures[key] = failure
	}
	block := s.blocks[key]
	mode := s.hash
	s.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-r.Context().Done():
			return
		}
	}
	if shouldFail {
		// A depleted hook is removed, so its final selected attempt still fails.
		s.mu.Lock()
		if failure.Remaining == 0 {
			delete(s.failures, key)
		}
		s.mu.Unlock()
		if failure.Disconnect {
			connection, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = connection.Close()
			}
			return
		}
		s.failHTTP(w, failure.Status, "SlowDown", "Transient fixture failure")
		return
	}
	if checksum := r.Header.Get("X-Amz-Checksum-Sha256"); checksum != "" {
		sum := sha256.Sum256(body)
		if base64.StdEncoding.EncodeToString(sum[:]) != checksum {
			s.failHTTP(w, 400, "BadDigest", "SHA256 mismatch")
			return
		}
	}
	if checksum := r.Header.Get("X-Amz-Content-Sha256"); len(checksum) == 64 {
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != checksum {
			s.failHTTP(w, 400, "BadDigest", "SHA256 mismatch")
			return
		}
	}
	value := Object{Body: body, ContentType: r.Header.Get("Content-Type"), ETag: etag(mode, body)}
	s.mu.Lock()
	s.objects[full] = value
	s.mu.Unlock()
	w.Header().Set("ETag", `"`+value.ETag+`"`)
	w.WriteHeader(http.StatusOK)
}

type listResult struct {
	XMLName               xml.Name     `xml:"ListBucketResult"`
	XMLNS                 string       `xml:"xmlns,attr"`
	Name                  string       `xml:"Name"`
	Prefix                string       `xml:"Prefix"`
	KeyCount              int          `xml:"KeyCount"`
	MaxKeys               int          `xml:"MaxKeys"`
	IsTruncated           bool         `xml:"IsTruncated"`
	NextContinuationToken string       `xml:"NextContinuationToken,omitempty"`
	Contents              []listObject `xml:"Contents"`
}
type listObject struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int    `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
}

func (s *Store) list(w http.ResponseWriter, r *http.Request, bucket string) {
	prefix := r.URL.Query().Get("prefix")
	start, _ := strconv.Atoi(r.URL.Query().Get("continuation-token"))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.List++
	keys := []string{}
	for full := range s.objects {
		if key, ok := strings.CutPrefix(full, bucket+"/"); ok && strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	limit := s.pageSize
	if requested, err := strconv.Atoi(r.URL.Query().Get("max-keys")); err == nil && requested < limit {
		limit = requested
	}
	if start < 0 || start > len(keys) || limit < 1 {
		s.failHTTP(w, 400, "InvalidArgument", "Invalid page")
		return
	}
	end := min(start+limit, len(keys))
	result := listResult{XMLNS: "http://s3.amazonaws.com/doc/2006-03-01/", Name: bucket, Prefix: prefix, MaxKeys: limit, KeyCount: end - start, IsTruncated: end < len(keys)}
	if result.IsTruncated {
		result.NextContinuationToken = strconv.Itoa(end)
	}
	for _, key := range keys[start:end] {
		object := s.objects[bucket+"/"+key]
		result.Contents = append(result.Contents, listObject{Key: key, ETag: `"` + object.ETag + `"`, Size: len(object.Body), StorageClass: "STANDARD", LastModified: "2023-11-14T22:13:20.000Z"})
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(result)
}
func (s *Store) failHTTP(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "<Error><Code>%s</Code><Message>%s</Message></Error>", code, message)
}
