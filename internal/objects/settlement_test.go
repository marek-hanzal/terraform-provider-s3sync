package objects

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/marek-hanzal/terraform-provider-s3sync/internal/teststore"
)

func TestFailedSyncDrainsAdmittedSuccessAndResumesFromPartialInventory(t *testing.T) {
	store := teststore.New(t)
	client := testClient(t, store, "opaque")
	transport := client.api
	secondStarted := make(chan struct{})
	failureReturned := make(chan struct{})
	release := make(chan struct{})
	client.api = putHook{transport, func(ctx context.Context, input *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
		if aws.ToString(input.Key) == "a-failed" {
			<-secondStarted
			close(failureReturned)
			return nil, &smithy.GenericAPIError{Code: "AccessDenied", Message: "private SDK message"}
		}
		close(secondStarted)
		<-release
		return transport.PutObject(ctx, input)
	}}
	request := Request{
		Bucket: "bucket", Concurrency: 2,
		Files:    map[string]File{"a-failed": fixtureFile(t, []byte("a"), ""), "b-admitted": fixtureFile(t, []byte("b"), "")},
		Previous: map[string]Record{"old-owned": {ETag: "old", SHA256: "old", Size: 3}},
	}
	type outcome struct {
		result Result
		err    error
	}
	finished := make(chan outcome, 1)
	go func() {
		result, err := client.Reconcile(context.Background(), request)
		finished <- outcome{result, err}
	}()
	select {
	case <-failureReturned:
	case <-time.After(3 * time.Second):
		t.Fatal("both bounded workers were not admitted")
	}
	select {
	case <-finished:
		t.Fatal("failed sync returned while admitted work was still unsettled")
	default:
	}
	close(release)
	var settled outcome
	select {
	case settled = <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("admitted successful work was canceled or never settled")
	}
	if settled.err == nil || settled.result.Uploaded != 1 || len(settled.result.Inventory) != 2 {
		t.Fatalf("partial settlement lost ownership: %+v %v", settled.result, settled.err)
	}
	if _, owned := settled.result.Inventory["a-failed"]; owned {
		t.Fatal("unacknowledged failed object was added to inventory")
	}
	client.api = transport
	request.Previous = settled.result.Inventory
	store.ResetStats()
	resumed, err := client.Reconcile(context.Background(), request)
	if err != nil || resumed.Uploaded != 1 || resumed.Skipped != 1 || store.Stats().PutByKey["b-admitted"] != 0 {
		t.Fatalf("resume replayed acknowledged success: %+v %+v %v", resumed, store.Stats(), err)
	}
}

func TestParentCancellationStopsAdmissionAndDoesNotInventAcknowledgements(t *testing.T) {
	store := teststore.New(t)
	client := testClient(t, store, "opaque")
	started := make(chan struct{})
	var uploads atomic.Int32
	client.api = putHook{client.api, func(ctx context.Context, input *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
		uploads.Add(1)
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		result, err := client.Reconcile(ctx, Request{
			Bucket: "bucket", Concurrency: 1,
			Files: map[string]File{"a": fixtureFile(t, []byte("a"), ""), "b": fixtureFile(t, []byte("b"), "")},
		})
		if len(result.Inventory) != 0 {
			finished <- &smithy.GenericAPIError{Code: "InventedAcknowledgement"}
			return
		}
		finished <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("upload never started")
	}
	cancel()
	select {
	case err := <-finished:
		if err == nil || !strings.Contains(err.Error(), "canceled") || uploads.Load() != 1 {
			t.Fatalf("cancellation boundary: uploads=%d err=%v", uploads.Load(), err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("parent cancellation did not settle sync")
	}
}

func TestMissingETagResponseDoesNotAcknowledgeAnUnprovableUpload(t *testing.T) {
	store := teststore.New(t)
	client := testClient(t, store, "opaque")
	client.api = putHook{client.api, func(context.Context, *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
		return &s3.PutObjectOutput{}, nil
	}}
	result, err := client.Reconcile(context.Background(), Request{
		Bucket: "bucket", Files: map[string]File{"asset": fixtureFile(t, []byte("body"), "")},
	})
	if err == nil || !strings.Contains(err.Error(), "missing_etag_acknowledgement") || len(result.Inventory) != 0 {
		t.Fatalf("missing ETag was acknowledged: %+v %v", result, err)
	}
}

type putHook struct {
	s3API
	put func(context.Context, *s3.PutObjectInput) (*s3.PutObjectOutput, error)
}

func (hook putHook) PutObject(ctx context.Context, input *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	return hook.put(ctx, input)
}
