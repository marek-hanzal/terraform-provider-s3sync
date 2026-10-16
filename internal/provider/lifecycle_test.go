package provider_test

import (
	"bytes"
	"fmt"
	"github.com/marek-hanzal/terraform-provider-s3sync/internal/teststore"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// These scenarios run Terraform itself: state refresh, saved plans, dependency
// ordering and partial failure cannot be proved by calling resource methods.
func TestTerraformLifecycle(t *testing.T) {
	binary := buildProvider(t)
	t.Run("relative_sources_survive_root_relocation_without_consumer_replacement", func(t *testing.T) {
		store := teststore.New(t)
		tf := newTerraform(t, binary, store)
		tf.sourceDirectory = filepath.Join(tf.dir, "inputs")
		tf.file("a.bin", "alpha", "application/octet-stream")
		tf.consumer = true
		tf.writeConfig()
		tf.run(0, "init", "-input=false")
		tf.apply()
		assertBody(t, store, "assets/a.bin", "alpha")
		consumer := tf.run(0, "state", "show", "terraform_data.consumer")

		second := t.TempDir()
		if err := os.WriteFile(filepath.Join(second, "a.bin"), []byte("alpha"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(tf.sourceDirectory); err != nil {
			t.Fatal(err)
		}
		tf.sourceDirectory = second
		tf.writeConfig()
		store.ResetStats()
		tf.plan(0, "relocated.plan")
		tf.run(0, "apply", "-input=false", "-auto-approve", "relocated.plan")
		assertNoWrites(t, store.Stats())
		if current := tf.run(0, "state", "show", "terraform_data.consumer"); current != consumer {
			t.Fatal("changing only the provider-local root replaced its publication consumer")
		}

		// A real body change must use the new root, and source verification must
		// still precede writes even for a previously acknowledged matching body.
		if err := os.WriteFile(filepath.Join(second, "a.bin"), []byte("ALPHA"), 0600); err != nil {
			t.Fatal(err)
		}
		store.ResetStats()
		tf.concurrency = 3
		tf.writeConfig()
		output := tf.run(1, "apply", "-input=false", "-auto-approve")
		assertNoWrites(t, store.Stats())
		if !strings.Contains(output, "sha256_mismatch") {
			t.Fatalf("relative source mismatch lost its checksum diagnostic: %s", output)
		}
		tf.file("a.bin", "ALPHA", "application/octet-stream")
		tf.writeConfig()
		store.ResetStats()
		tf.apply()
		if stats := store.Stats(); stats.Put != 1 || stats.Delete != 0 {
			t.Fatalf("changed relative source must upload exactly once: %+v", stats)
		}
		assertBody(t, store, "assets/a.bin", "ALPHA")
	})
	t.Run("diff_drift_saved_plan_and_retention", func(t *testing.T) {
		store := teststore.New(t)
		tf := newTerraform(t, binary, store)
		tf.file("a.bin", "alpha", "application/octet-stream")
		tf.file("b.bin", "bravo", "application/octet-stream")
		tf.documents = true
		tf.writeConfig()
		tf.run(0, "init", "-input=false")
		store.ResetStats()
		tf.plan(2, "cold.plan")
		assertNoWrites(t, store.Stats())
		tf.run(0, "apply", "-input=false", "-auto-approve", "cold.plan")
		if got := store.Stats().Put; got != 3 {
			t.Fatalf("cold upload PUTs=%d, want 3", got)
		}
		assertBody(t, store, "assets/a.bin", "alpha")
		assertBody(t, store, "site/index.html", "index-v1")

		store.ResetStats()
		tf.plan(0, "unchanged.plan")
		tf.run(0, "apply", "-input=false", "-auto-approve", "unchanged.plan")
		stats := store.Stats()
		assertNoWrites(t, stats)
		if stats.List > 6 || stats.Head != 0 {
			t.Fatalf("unchanged files should use bounded list requests without per-object HEAD: %+v", stats)
		}

		tf.file("a.bin", "ALPHA", "application/octet-stream")
		tf.writeConfig()
		store.ResetStats()
		tf.apply()
		stats = store.Stats()
		if stats.Put != 1 || stats.PutByKey["assets/a.bin"] != 1 {
			t.Fatalf("same-size local change should upload one object: %+v", stats)
		}
		assertBody(t, store, "assets/a.bin", "ALPHA")

		tf.concurrency = 3
		tf.maxAttempts = 4
		tf.writeConfig()
		store.ResetStats()
		tf.apply()
		assertNoWrites(t, store.Stats())

		store.Remove(fixtureBucket, "assets/a.bin")
		store.Seed(fixtureBucket, "assets/b.bin", []byte("BROKE"), "application/octet-stream")
		store.ResetStats()
		tf.plan(2, "drift.plan")
		assertNoWrites(t, store.Stats())
		tf.run(0, "apply", "-input=false", "-auto-approve", "drift.plan")
		stats = store.Stats()
		if stats.Put != 2 {
			t.Fatalf("missing and changed remote files should both repair: %+v", stats)
		}
		assertBody(t, store, "assets/b.bin", "bravo")

		tf.file("a.bin", "again", "application/octet-stream")
		tf.writeConfig()
		tf.plan(2, "stale.plan")
		if err := os.WriteFile(tf.files["a.bin"].source, []byte("raced"), 0600); err != nil {
			t.Fatal(err)
		}
		store.ResetStats()
		output := tf.run(1, "apply", "-input=false", "-auto-approve", "stale.plan")
		assertNoWrites(t, store.Stats())
		if !strings.Contains(strings.ToLower(output), "sha256") && !strings.Contains(strings.ToLower(output), "checksum") {
			t.Fatalf("saved-plan source mismatch needs a checksum diagnostic: %s", output)
		}
		tf.file("a.bin", "again", "application/octet-stream")
		tf.writeConfig()
		tf.apply()

		delete(tf.files, "b.bin")
		tf.writeConfig()
		store.ResetStats()
		tf.apply()
		if _, ok := store.Object(fixtureBucket, "assets/b.bin"); !ok {
			t.Fatal("removing a file from the set must retain it until explicit prune")
		}
		if store.Stats().Delete != 0 {
			t.Fatal("file set update unexpectedly deleted removed file")
		}
		tf.run(0, "destroy", "-input=false", "-auto-approve")
		assertBody(t, store, "assets/a.bin", "again")
		assertBody(t, store, "site/index.html", "index-v1")
	})
	t.Run("pagination_and_existing_content_adoption", func(t *testing.T) {
		store := teststore.New(t)
		tf := newTerraform(t, binary, store)
		// 1,001 remote objects force the real SDK paginator past its first page;
		// only one is desired, so the assertion also protects prefix ownership.
		for i := 0; i < 1001; i++ {
			store.Seed(fixtureBucket, fmt.Sprintf("assets/old-%04d.bin", i), []byte("old"), "application/octet-stream")
		}
		tf.file("same.bin", "same", "application/octet-stream")
		store.Seed(fixtureBucket, "assets/same.bin", []byte("same"), "application/octet-stream")
		tf.writeConfig()
		tf.run(0, "init", "-input=false")
		store.ResetStats()
		tf.apply()
		stats := store.Stats()
		if stats.List < 2 {
			t.Fatalf("1,002 objects must consume at least two ListObjectsV2 pages: %+v", stats)
		}
		if stats.Put != 0 {
			t.Fatalf("explicit SHA256 ETags must adopt matching existing contents: %+v", stats)
		}
		if stats.Head != 1 {
			t.Fatalf("adoption must verify content type once: %+v", stats)
		}
		store.ResetStats()
		tf.plan(0, "nochange.plan")
		assertNoWrites(t, store.Stats())
		if store.Stats().Head != 0 {
			t.Fatal("acknowledged inventory should not issue per-object HEAD")
		}
	})
	t.Run("retry_failure_recovery_and_publication_barrier", func(t *testing.T) {
		store := teststore.New(t)
		store.SetETagHash("opaque")
		tf := newTerraform(t, binary, store)
		tf.etagHash = ""
		tf.maxAttempts = 2
		tf.file("settled.bin", "already-acknowledged", "application/octet-stream")
		tf.writeConfig()
		tf.run(0, "init", "-input=false")
		tf.apply()
		tf.file("accepted.bin", "acknowledged-during-failure", "application/octet-stream")
		tf.file("retry.bin", "entire-replayed-body", "application/octet-stream")
		tf.documents = true
		tf.prune = true
		store.Seed(fixtureBucket, "assets/obsolete.bin", []byte("obsolete"), "application/octet-stream")
		store.Seed(fixtureBucket, "unrelated/preserve.bin", []byte("preserve"), "application/octet-stream")
		tf.writeConfig()
		store.Fail("assets/retry.bin", teststore.Failure{Remaining: 2, Status: 503})
		store.ResetStats()
		tf.run(1, "apply", "-input=false", "-auto-approve")
		stats := store.Stats()
		if stats.PutByKey["assets/retry.bin"] != 2 || stats.PutByKey["assets/accepted.bin"] != 1 || stats.PutByKey["assets/settled.bin"] != 0 || stats.PutByKey["site/index.html"] != 0 || stats.Delete != 0 {
			t.Fatalf("exhausted retries must block dependent documents and prune: %+v", stats)
		}
		for _, body := range stats.Bodies["assets/retry.bin"] {
			if string(body) != "entire-replayed-body" {
				t.Fatalf("retry consumed an exhausted stream: %q", body)
			}
		}
		if _, ok := store.Object(fixtureBucket, "assets/obsolete.bin"); !ok {
			t.Fatal("failed publication pruned old content")
		}

		// The next apply is a real recovery after Terraform has persisted partial
		// state, not a fresh resource instance in a unit test.
		store.Fail("assets/retry.bin", teststore.Failure{Remaining: 1, Disconnect: true})
		store.Fail("site/index.html", teststore.Failure{Remaining: 2, Status: 503})
		store.ResetStats()
		tf.run(1, "apply", "-input=false", "-auto-approve")
		stats = store.Stats()
		if stats.PutByKey["assets/retry.bin"] != 2 || stats.PutByKey["assets/accepted.bin"] != 0 || stats.PutByKey["assets/settled.bin"] != 0 || stats.PutByKey["site/index.html"] != 2 || stats.Delete != 0 {
			t.Fatalf("partial inventory must survive; document failure must block prune: %+v", stats)
		}
		for _, body := range stats.Bodies["assets/retry.bin"] {
			if string(body) != "entire-replayed-body" {
				t.Fatalf("lost-connection retry body=%q", body)
			}
		}
		if _, ok := store.Object(fixtureBucket, "assets/obsolete.bin"); !ok {
			t.Fatal("failed documents must preserve obsolete files")
		}
		store.ResetStats()
		tf.apply()
		stats = store.Stats()
		if stats.Put != 1 || stats.PutByKey["site/index.html"] != 1 {
			t.Fatalf("recovery must upload only previously failed documents: %+v", stats)
		}
		if _, ok := store.Object(fixtureBucket, "assets/obsolete.bin"); ok {
			t.Fatal("successful prerequisite documents did not permit explicit prune")
		}
		assertBody(t, store, "unrelated/preserve.bin", "preserve")
		assertBody(t, store, "site/index.html", "index-v1")
		store.ResetStats()
		tf.plan(0, "settled.plan")
		assertNoWrites(t, store.Stats())
	})
	t.Run("etag_contracts", func(t *testing.T) {
		for _, mode := range []string{"md5", "opaque"} {
			t.Run(mode, func(t *testing.T) {
				store := teststore.New(t)
				store.SetETagHash(mode)
				tf := newTerraform(t, binary, store)
				tf.etagHash = mode
				if mode == "opaque" {
					tf.etagHash = ""
				}
				tf.file("existing.bin", "existing", "application/octet-stream")
				store.Seed(fixtureBucket, "assets/existing.bin", []byte("existing"), "application/octet-stream")
				tf.writeConfig()
				tf.run(0, "init", "-input=false")
				store.ResetStats()
				tf.apply()
				want := 0
				if mode == "opaque" {
					want = 1
				}
				if got := store.Stats().Put; got != want {
					t.Fatalf("%s ETag adoption PUTs=%d, want %d", mode, got, want)
				}
				store.ResetStats()
				tf.plan(0, "acknowledged.plan")
				assertNoWrites(t, store.Stats())
				if store.Stats().Head != 0 {
					t.Fatal("known inventory must remain list-only with any ETag mode")
				}
			})
		}
	})
	t.Run("cancellation_stops_in_flight_upload", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("os.Interrupt cannot signal subprocesses on Windows")
		}
		store := teststore.New(t)
		tf := newTerraform(t, binary, store)
		tf.file("blocked.bin", "body", "application/octet-stream")
		tf.documents = true
		tf.writeConfig()
		tf.run(0, "init", "-input=false")
		release := make(chan struct{})
		defer close(release)
		store.Block("assets/blocked.bin", release)
		cmd, cancel := tf.command("apply", "-input=false", "-auto-approve")
		defer cancel()
		var output bytes.Buffer
		cmd.Stdout, cmd.Stderr = &output, &output
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		deadline := time.Now().Add(10 * time.Second)
		for store.Stats().Put == 0 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if store.Stats().Put == 0 {
			_ = cmd.Process.Kill()
			<-done
			t.Fatalf("upload did not start: %s", output.String())
		}
		if err := cmd.Process.Signal(os.Interrupt); err != nil {
			_ = cmd.Process.Kill()
			<-done
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("cancelled apply unexpectedly succeeded")
			}
		case <-time.After(8 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			t.Fatalf("Terraform cancellation did not stop an in-flight request promptly: %s", output.String())
		}
		if store.Stats().PutByKey["site/index.html"] != 0 {
			t.Fatal("cancellation published dependent documents")
		}
		if bytes.Contains(output.Bytes(), []byte(fixtureSecretKey)) || bytes.Contains(output.Bytes(), []byte(fixtureAccessKey)) {
			t.Fatal("cancellation diagnostics exposed credentials")
		}
	})
	t.Run("opted_in_destroy_deletes_only_acknowledged_inventory", func(t *testing.T) {
		store := teststore.New(t)
		tf := newTerraform(t, binary, store)
		tf.deleteOnDestroy = true
		tf.file("owned.bin", "owned", "application/octet-stream")
		store.Seed(fixtureBucket, "assets/untracked.bin", []byte("untracked"), "application/octet-stream")
		store.Seed(fixtureBucket, "elsewhere/keep.bin", []byte("keep"), "application/octet-stream")
		tf.writeConfig()
		tf.run(0, "init", "-input=false")
		tf.apply()
		store.ResetStats()
		tf.run(0, "destroy", "-input=false", "-auto-approve")
		stats := store.Stats()
		if stats.Delete != 1 || stats.DeleteByKey["assets/owned.bin"] != 1 {
			t.Fatalf("destroy must delete only the acknowledged file: %+v", stats)
		}
		assertBody(t, store, "assets/untracked.bin", "untracked")
		assertBody(t, store, "elsewhere/keep.bin", "keep")
	})
}
