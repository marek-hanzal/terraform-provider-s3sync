package objects

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marek-hanzal/terraform-provider-s3sync/internal/teststore"
)

func TestBucketPropagationRetriesOnlyNoSuchBucketWithinConfiguredBudget(t *testing.T) {
	for _, scenario := range []struct {
		name, code                   string
		status, failures, max, calls int
		succeeds                     bool
	}{
		{"becomes_available", "NoSuchBucket", 404, 2, 3, 3, true},
		{"unavailable_after_budget", "NoSuchBucket", 404, 10, 3, 3, false},
		{"retry_disabled", "NoSuchBucket", 404, 10, 1, 1, false},
		{"missing_object", "NoSuchKey", 404, 10, 3, 1, false},
		{"generic_not_found", "NotFound", 404, 10, 3, 1, false},
		{"permission_denied", "AccessDenied", 403, 10, 3, 1, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			store := teststore.New(t)
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && int(calls.Add(1)) <= scenario.failures {
					w.Header().Set("Content-Type", "application/xml")
					w.WriteHeader(scenario.status)
					_, _ = fmt.Fprintf(w, "<Error><Code>%s</Code></Error>", scenario.code)
					return
				}
				store.Server.Config.Handler.ServeHTTP(w, r)
			}))
			t.Cleanup(server.Close)
			client, err := New(context.Background(), Config{Endpoint: server.URL, AccessKey: "fixture", SecretKey: "fixture", MaxAttempts: scenario.max, RequestTimeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.Reconcile(context.Background(), Request{Bucket: "bucket", Files: map[string]File{"asset": fixtureFile(t, []byte("body"), "text/plain")}, Concurrency: 1})
			if int(calls.Load()) != scenario.calls {
				t.Fatalf("list attempts=%d want=%d", calls.Load(), scenario.calls)
			}
			if scenario.succeeds {
				if err != nil || result.Uploaded != 1 || store.Stats().Put != 1 {
					t.Fatalf("propagated bucket did not complete upload: result=%+v err=%v stats=%+v", result, err, store.Stats())
				}
			} else if err == nil || !strings.Contains(err.Error(), scenario.code) || store.Stats().Put != 0 {
				t.Fatalf("unavailable destination must fail before uploads: err=%v stats=%+v", err, store.Stats())
			}
		})
	}
}

func TestBucketPropagationDuringPutReplaysTheWholeFile(t *testing.T) {
	store := teststore.New(t)
	var attempts atomic.Int64
	var mutation sync.Mutex
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			body, _ := io.ReadAll(r.Body)
			mutation.Lock()
			bodies = append(bodies, body)
			mutation.Unlock()
			r.Body = io.NopCloser(bytes.NewReader(body))
			if attempts.Add(1) == 1 {
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, "<Error><Code>NoSuchBucket</Code></Error>")
				return
			}
		}
		store.Server.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	client, err := New(context.Background(), Config{Endpoint: server.URL, AccessKey: "fixture", SecretKey: "fixture", MaxAttempts: 3, RequestTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	body := bytes.Repeat([]byte("bucket-propagation-body"), 32768)
	result, err := client.Reconcile(context.Background(), Request{Bucket: "bucket", Files: map[string]File{"asset": fixtureFile(t, body, "")}, Concurrency: 1})
	mutation.Lock()
	defer mutation.Unlock()
	if err != nil || result.Uploaded != 1 || attempts.Load() != 2 || len(bodies) != 2 {
		t.Fatalf("PUT retry result=%+v attempts=%d err=%v", result, attempts.Load(), err)
	}
	for _, actual := range bodies {
		if !bytes.Equal(actual, body) {
			t.Fatal("NoSuchBucket retry reused an exhausted body")
		}
	}
}
