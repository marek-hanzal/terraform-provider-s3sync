package objects

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/smithy-go"
	"github.com/marek-hanzal/terraform-provider-s3sync/internal/teststore"
)

func TestPaginatedListingAndExclusivePrunePreserveOtherOwners(t *testing.T) {
	store := teststore.New(t)
	store.SetPageSize(2)
	for _, key := range []string{"owned/a", "owned/b", "owned/c", "owned/d", "elsewhere/e"} {
		store.Seed("bucket", key, []byte(key), "text/plain")
	}
	client := testClient(t, store, "opaque")
	remote, err := client.List(context.Background(), "bucket", "owned/")
	if err != nil || len(remote) != 4 || store.Stats().List != 2 {
		t.Fatalf("pagination: objects=%d stats=%+v err=%v", len(remote), store.Stats(), err)
	}
	deleted, err := client.Prune(context.Background(), "bucket", "owned/", []string{"b"})
	if err != nil || deleted != 3 || store.Stats().Delete != 3 {
		t.Fatalf("prune: deleted=%d stats=%+v err=%v", deleted, store.Stats(), err)
	}
	for _, key := range []string{"owned/b", "elsewhere/e"} {
		if _, exists := store.Object("bucket", key); !exists {
			t.Fatalf("prune deleted retained object %s", key)
		}
	}
	if err := client.Remove(context.Background(), "bucket", "owned/", map[string]Record{"b": {}}); err != nil {
		t.Fatal(err)
	}
	if _, exists := store.Object("bucket", "elsewhere/e"); !exists {
		t.Fatal("resource destroy deleted another owner's object")
	}
}

func TestDiagnosticsNeverExposeSDKMessagesURLsOrArbitraryCodes(t *testing.T) {
	secret := "super-secret-credential"
	for _, source := range []error{
		errors.New("https://" + secret + "@endpoint/path?token=" + secret),
		&smithy.GenericAPIError{Code: secret, Message: "https://endpoint/" + secret},
	} {
		err := safeError("upload", "asset.png", source)
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "endpoint") || !strings.Contains(err.Error(), "asset.png") {
			t.Fatalf("unsafe or anonymous diagnostic: %s", err)
		}
	}
	err := safeError("upload", "asset", &smithy.GenericAPIError{Code: "AccessDenied", Message: secret})
	if !strings.Contains(err.Error(), "AccessDenied") || strings.Contains(err.Error(), secret) {
		t.Fatalf("safe service code was lost: %s", err)
	}
}

func TestAWSDefaultEndpointDoesNotRequireCustomOrigin(t *testing.T) {
	if _, err := New(context.Background(), Config{AccessKey: "fixture", SecretKey: "fixture"}); err != nil {
		t.Fatalf("default AWS endpoint was rejected: %v", err)
	}
}

func testClient(t *testing.T, store *teststore.Store, hash string) *Client {
	t.Helper()
	client, err := New(context.Background(), Config{
		Endpoint: store.URL(), AccessKey: "fixture-access", SecretKey: "fixture-secret",
		ETagHash: hash, MaxAttempts: 3, RequestTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}
