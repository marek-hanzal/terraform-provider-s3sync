package provider_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/marek-hanzal/terraform-provider-s3sync/internal/teststore"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

const (
	fixtureBucket    = "fixture-bucket"
	fixtureAccessKey = "fixture-access-key-sensitive"
	fixtureSecretKey = "fixture-secret-key-sensitive"
)

type localFile struct{ source, hash, contentType string }
type terraformFixture struct {
	t                                              *testing.T
	dir                                            string
	store                                          *teststore.Store
	files                                          map[string]localFile
	concurrency, maxAttempts                       int
	documents, prune, deleteOnDestroy              bool
	etagHash                                       string
	pruneKeyPattern                                string
	endpoint, accessKey, secretKey, bucket, region string
}

func buildProvider(t *testing.T) string {
	t.Helper()
	if binary := os.Getenv("S3SYNC_TEST_PROVIDER_BINARY"); binary != "" {
		if !filepath.IsAbs(binary) {
			t.Fatal("S3SYNC_TEST_PROVIDER_BINARY must be absolute")
		}
		return binary
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "terraform-provider-s3sync")
	cmd := exec.Command("go", "build", "-mod=readonly", "-o", binary, ".")
	cmd.Dir = root
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build provider: %v\n%s", err, output)
	}
	return binary
}
func newTerraform(t *testing.T, binary string, store *teststore.Store) *terraformFixture {
	t.Helper()
	dir := t.TempDir()
	mirror := filepath.Join(dir, "mirror")
	platform := runtime.GOOS + "_" + runtime.GOARCH
	packageDir := filepath.Join(mirror, "registry.terraform.io", "marek-hanzal", "s3sync", "0.1.0", platform)
	if err := os.MkdirAll(packageDir, 0700); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageDir, "terraform-provider-s3sync_v0.1.0"), body, 0700); err != nil {
		t.Fatal(err)
	}
	cli := fmt.Sprintf("provider_installation { filesystem_mirror { path = %q include = [\"registry.terraform.io/marek-hanzal/s3sync\"] } }\n", mirror)
	if err := os.WriteFile(filepath.Join(dir, "terraform.rc"), []byte(cli), 0600); err != nil {
		t.Fatal(err)
	}
	endpoint := ""
	if store != nil {
		endpoint = store.URL()
	}
	return &terraformFixture{t: t, dir: dir, store: store, files: map[string]localFile{}, concurrency: 8, maxAttempts: 5, etagHash: "sha256", endpoint: endpoint, accessKey: fixtureAccessKey, secretKey: fixtureSecretKey, bucket: fixtureBucket, region: "us-east-1"}
}
func (tf *terraformFixture) file(key, body, contentType string) {
	tf.t.Helper()
	source := filepath.Join(tf.dir, "inputs", filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(source), 0700); err != nil {
		tf.t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte(body), 0600); err != nil {
		tf.t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(body))
	tf.files[key] = localFile{source: source, hash: hex.EncodeToString(hash[:]), contentType: contentType}
}
func (tf *terraformFixture) writeConfig() {
	tf.t.Helper()
	files := map[string]any{}
	keep := []string{}
	for key, file := range tf.files {
		files[key] = map[string]any{"source": file.source, "sha256": file.hash, "content_type": file.contentType}
		keep = append(keep, key)
	}
	resources := map[string]any{"s3sync_files": map[string]any{"assets": map[string]any{"bucket": tf.bucket, "prefix": "assets/", "files": files, "concurrency": tf.concurrency, "delete_on_destroy": tf.deleteOnDestroy}}}
	if tf.documents {
		source := filepath.Join(tf.dir, "index.html")
		if err := os.WriteFile(source, []byte("index-v1"), 0600); err != nil {
			tf.t.Fatal(err)
		}
		hash := sha256.Sum256([]byte("index-v1"))
		resources["s3sync_files"].(map[string]any)["documents"] = map[string]any{"bucket": tf.bucket, "prefix": "site/", "files": map[string]any{"index.html": map[string]any{"source": source, "sha256": hex.EncodeToString(hash[:]), "content_type": "text/html"}}, "depends_on": []string{"s3sync_files.assets"}}
	}
	if tf.prune {
		prune := map[string]any{"bucket": tf.bucket, "prefix": "assets/", "keep": keep, "depends_on": []string{"s3sync_files.documents"}}
		if tf.pruneKeyPattern != "" {
			prune["key_pattern"] = tf.pruneKeyPattern
		}
		resources["s3sync_prune"] = map[string]any{"obsolete": prune}
	}
	config := map[string]any{"terraform": map[string]any{"required_providers": map[string]any{"s3sync": map[string]any{"source": "marek-hanzal/s3sync", "version": "= 0.1.0"}}}, "provider": map[string]any{"s3sync": map[string]any{"endpoint": tf.endpoint, "region": tf.region, "access_key": tf.accessKey, "secret_key": tf.secretKey, "etag_hash": tf.etagHash, "max_attempts": tf.maxAttempts, "request_timeout_seconds": 20}}, "resource": resources}
	if tf.etagHash == "" {
		delete(config["provider"].(map[string]any)["s3sync"].(map[string]any), "etag_hash")
	}
	body, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		tf.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tf.dir, "main.tf.json"), body, 0600); err != nil {
		tf.t.Fatal(err)
	}
}
func (tf *terraformFixture) command(args ...string) (*exec.Cmd, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	cmd := exec.CommandContext(ctx, "terraform", append([]string{"-chdir=" + tf.dir}, args...)...)
	cmd.Env = append(os.Environ(), "TF_CLI_CONFIG_FILE="+filepath.Join(tf.dir, "terraform.rc"), "CHECKPOINT_DISABLE=1", "TF_IN_AUTOMATION=1", "TF_LOG=")
	return cmd, cancel
}
func (tf *terraformFixture) run(want int, args ...string) string {
	tf.t.Helper()
	cmd, cancel := tf.command(args...)
	defer cancel()
	output, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else {
			tf.t.Fatalf("terraform %v: %v", args, err)
		}
	}
	if bytes.Contains(output, []byte(tf.accessKey)) || bytes.Contains(output, []byte(tf.secretKey)) {
		tf.t.Fatalf("terraform diagnostics exposed credentials: %s", output)
	}
	if code != want {
		tf.t.Fatalf("terraform %v exit=%d want=%d\n%s", args, code, want, output)
	}
	return string(output)
}
func (tf *terraformFixture) apply() {
	tf.t.Helper()
	tf.run(0, "apply", "-input=false", "-auto-approve")
}
func (tf *terraformFixture) plan(code int, name string) {
	tf.t.Helper()
	tf.run(code, "plan", "-input=false", "-detailed-exitcode", "-out="+name)
}
func assertNoWrites(t *testing.T, stats teststore.Stats) {
	t.Helper()
	if stats.Put != 0 || stats.Delete != 0 {
		t.Fatalf("plan/unchanged apply wrote remote storage: %+v", stats)
	}
}
func assertBody(t *testing.T, store *teststore.Store, key, want string) {
	t.Helper()
	object, ok := store.Object(fixtureBucket, key)
	if !ok || string(object.Body) != want {
		t.Fatalf("object %q body=%q exists=%v, want %q", key, object.Body, ok, want)
	}
}
