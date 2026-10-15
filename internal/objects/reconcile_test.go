package objects

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marek-hanzal/terraform-provider-s3sync/internal/teststore"
)

func TestAcknowledgedDiffUsesBulkListingAndRepairsBodyOrDesiredTypeDrift(t *testing.T) {
	store := teststore.New(t)
	store.SetETagHash("opaque")
	store.Seed("bucket", "files/unrelated", []byte("keep"), "text/plain")
	client := testClient(t, store, "opaque")
	files := map[string]File{"a": fixtureFile(t, []byte("aaaa"), "text/plain"), "b": fixtureFile(t, []byte("bbbb"), "text/plain")}
	request := Request{Bucket: "bucket", Prefix: "files/", Files: files}
	first, err := client.Reconcile(context.Background(), request)
	if err != nil || first.Uploaded != 2 {
		t.Fatalf("initial sync: %+v %v", first, err)
	}
	request.Previous = first.Inventory
	store.ResetStats()
	second, err := client.Reconcile(context.Background(), request)
	if err != nil || second.Skipped != 2 || store.Stats().List != 1 || store.Stats().Head != 0 || store.Stats().Put != 0 {
		t.Fatalf("unchanged sync used per-object I/O: %+v %+v %v", second, store.Stats(), err)
	}
	store.Seed("bucket", "files/a", []byte("evil"), "text/plain") // Same size, different opaque ETag.
	b := files["b"]
	b.ContentType = "application/json"
	files["b"] = b
	store.ResetStats()
	third, err := client.Reconcile(context.Background(), request)
	if err != nil || third.Uploaded != 2 || store.Stats().Put != 2 {
		t.Fatalf("drift/type repair: %+v %+v %v", third, store.Stats(), err)
	}
	delete(files, "a")
	request.Previous = third.Inventory
	fourth, err := client.Reconcile(context.Background(), request)
	if err != nil || len(fourth.Inventory) != 2 || store.Stats().Delete != 0 {
		t.Fatalf("removed desired files were not retained for ownership: %+v %v", fourth, err)
	}
	if err := client.Remove(context.Background(), "bucket", "files/", fourth.Inventory); err != nil {
		t.Fatal(err)
	}
	if _, exists := store.Object("bucket", "files/unrelated"); !exists {
		t.Fatal("destroy acquired ownership of unrelated objects")
	}
}

func TestExplicitHashAdoptionInspectsTypeAndOpaqueModeDoesNotAssumeSHA256(t *testing.T) {
	for _, hash := range []string{"sha256", "md5", "opaque"} {
		t.Run(hash, func(t *testing.T) {
			store := teststore.New(t)
			if hash != "opaque" {
				store.SetETagHash(hash)
			}
			body := []byte("existing body")
			store.Seed("bucket", "files/good", body, "text/plain")
			store.Seed("bucket", "files/wrong-type", body, "application/json")
			file := fixtureFile(t, body, "text/plain")
			result, err := testClient(t, store, hash).Reconcile(context.Background(), Request{
				Bucket: "bucket", Prefix: "files/", Files: map[string]File{"good": file, "wrong-type": file},
			})
			if err != nil {
				t.Fatal(err)
			}
			if hash == "opaque" {
				if result.Uploaded != 2 || store.Stats().Head != 0 {
					t.Fatalf("opaque ETag was interpreted as a hash: %+v %+v", result, store.Stats())
				}
			} else if result.Uploaded != 1 || result.Skipped != 1 || store.Stats().Head != 2 {
				t.Fatalf("hash adoption failed body/type boundary: %+v %+v", result, store.Stats())
			}
			object, _ := store.Object("bucket", "files/wrong-type")
			if object.ContentType != "text/plain" {
				t.Fatal("adopted an object with the wrong content type")
			}
		})
	}
}

func TestEverySourceIsVerifiedBeforeAnyUploadIncludingAcknowledgedFiles(t *testing.T) {
	store := teststore.New(t)
	client := testClient(t, store, "opaque")
	good := fixtureFile(t, []byte("good"), "")
	bad := fixtureFile(t, []byte("bad"), "")
	bad.SHA256 = good.SHA256
	request := Request{Bucket: "bucket", Files: map[string]File{"a-good": good, "z-bad": bad}}
	if _, err := client.Reconcile(context.Background(), request); err == nil || !strings.Contains(err.Error(), "z-bad") {
		t.Fatalf("expected named checksum failure, got %v", err)
	}
	if store.Stats().Put != 0 {
		t.Fatal("wrote before completing all source verification")
	}
	delete(request.Files, "z-bad")
	first, err := client.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	request.Previous = first.Inventory
	if err := os.WriteFile(good.Source, []byte("evil"), 0o600); err != nil {
		t.Fatal(err)
	}
	store.ResetStats()
	if _, err := client.Reconcile(context.Background(), request); err == nil || store.Stats().Put != 0 {
		t.Fatalf("acknowledged source bypassed hash verification: %v %+v", err, store.Stats())
	}
}

func TestNonregularSourceCannotBecomeAnEmptyRemoteFile(t *testing.T) {
	store := teststore.New(t)
	file := File{Source: t.TempDir(), SHA256: strings.Repeat("0", 64)}
	_, err := testClient(t, store, "opaque").Reconcile(context.Background(), Request{Bucket: "bucket", Files: map[string]File{"directory": file}})
	if err == nil || !strings.Contains(err.Error(), "nonregular_source") || store.Stats().Put != 0 {
		t.Fatalf("nonregular source admitted: %v %+v", err, store.Stats())
	}
}

func TestSDKRetriesReplayTheWholeSeekableFile(t *testing.T) {
	for _, failure := range []teststore.Failure{{Remaining: 1, Status: 503}, {Remaining: 1, Disconnect: true}} {
		name := "503"
		if failure.Disconnect {
			name = "disconnect"
		}
		t.Run(name, func(t *testing.T) {
			store := teststore.New(t)
			store.Fail("files/retry", failure)
			body := bytes.Repeat([]byte("whole-file-body"), 32768)
			result, err := testClient(t, store, "opaque").Reconcile(context.Background(), Request{
				Bucket: "bucket", Prefix: "files/", Files: map[string]File{"retry": fixtureFile(t, body, "")},
			})
			if err != nil || result.Uploaded != 1 {
				t.Fatalf("retry failed: %+v %v", result, err)
			}
			bodies := store.Stats().Bodies["files/retry"]
			if len(bodies) != 2 {
				t.Fatalf("wanted failed attempt plus retry, got %d", len(bodies))
			}
			for _, actual := range bodies {
				if !bytes.Equal(actual, body) {
					t.Fatal("retry reused a partially consumed body")
				}
			}
		})
	}
}

func fixtureFile(t *testing.T, body []byte, contentType string) File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	checksum := sha256.Sum256(body)
	return File{Source: path, SHA256: hex.EncodeToString(checksum[:]), ContentType: contentType}
}
