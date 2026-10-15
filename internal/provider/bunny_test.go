package provider_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// This opt-in regression owns fresh control-plane provisioning followed by
// unknown provider configuration becoming usable in the same Terraform apply.
// There is no external readiness request, waiter, sleep or retry around apply.
func TestTerraformFreshBunnyZone(t *testing.T) {
	if os.Getenv("S3SYNC_TEST_BUNNY") != "1" {
		t.Skip("set S3SYNC_TEST_BUNNY=1 with explicit Bunny test credentials and provider binary")
	}
	key := os.Getenv("BUNNYNET_API_KEY")
	bunnyBinary := os.Getenv("S3SYNC_TEST_BUNNY_PROVIDER_BINARY")
	if key == "" || !filepath.IsAbs(bunnyBinary) {
		t.Fatal("BUNNYNET_API_KEY and absolute S3SYNC_TEST_BUNNY_PROVIDER_BINARY are required")
	}
	syncBinary := buildProvider(t)
	versionBody, err := os.ReadFile(filepath.Join("..", "..", "VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	version := strings.TrimSpace(string(versionBody))
	for round := range 3 {
		t.Run(fmt.Sprint(round+1), func(t *testing.T) {
			dir := t.TempDir()
			platform := runtime.GOOS + "_" + runtime.GOARCH
			for _, item := range []struct{ address, version, binary, name string }{
				{"marek-hanzal/s3sync", version, syncBinary, "s3sync"},
				{"bunnyway/bunnynet", "0.19.1", bunnyBinary, "bunnynet"},
			} {
				packageDir := filepath.Join(dir, "mirror", "registry.terraform.io", item.address, item.version, platform)
				if err := os.MkdirAll(packageDir, 0700); err != nil {
					t.Fatal(err)
				}
				body, err := os.ReadFile(item.binary)
				if err != nil {
					t.Fatal("test provider binary unavailable")
				}
				if err := os.WriteFile(filepath.Join(packageDir, "terraform-provider-"+item.name+"_v"+item.version), body, 0700); err != nil {
					t.Fatal(err)
				}
			}
			cli := filepath.Join(dir, "terraform.rc")
			if err := os.WriteFile(cli, []byte(fmt.Sprintf("provider_installation { filesystem_mirror { path = %q } }\n", filepath.Join(dir, "mirror"))), 0600); err != nil {
				t.Fatal(err)
			}
			bucket := fmt.Sprintf("s3sync-readiness-%d", time.Now().UnixNano())
			body := "fresh Bunny zone without an external readiness gate"
			source := filepath.Join(dir, "ready.txt")
			if err := os.WriteFile(source, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256([]byte(body))
			config := map[string]any{
				"terraform": map[string]any{"required_providers": map[string]any{
					"bunnynet": map[string]any{"source": "BunnyWay/bunnynet", "version": "= 0.19.1"},
					"s3sync":   map[string]any{"source": "marek-hanzal/s3sync", "version": "= " + version},
				}},
				"provider": map[string]any{"s3sync": map[string]any{
					"endpoint": "https://${bunnynet_storage_zone.test.hostname_s3}", "region": "de",
					"access_key": "${bunnynet_storage_zone.test.name}", "secret_key": "${bunnynet_storage_zone.test.password}",
				}},
				"resource": map[string]any{
					"bunnynet_storage_zone": map[string]any{"test": map[string]any{"name": bucket, "region": "DE", "type": "S3", "zone_tier": "Standard", "replication_regions": []string{}}},
					"s3sync_files": map[string]any{"test": map[string]any{"bucket": "${bunnynet_storage_zone.test.name}", "prefix": "acceptance/", "files": map[string]any{
						"ready.txt": map[string]any{"source": source, "sha256": hex.EncodeToString(hash[:]), "content_type": "text/plain"},
					}}},
				},
				"output": map[string]any{"connection": map[string]any{"sensitive": true, "value": map[string]any{
					"id": "${bunnynet_storage_zone.test.id}", "endpoint": "https://${bunnynet_storage_zone.test.hostname_s3}", "password": "${bunnynet_storage_zone.test.password}",
				}}},
			}
			encoded, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "main.tf.json"), encoded, 0600); err != nil {
				t.Fatal(err)
			}
			run := func(args ...string) ([]byte, error) {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				cmd := exec.CommandContext(ctx, "terraform", append([]string{"-chdir=" + dir}, args...)...)
				cmd.Env = append(os.Environ(), "TF_CLI_CONFIG_FILE="+cli, "CHECKPOINT_DISABLE=1", "TF_LOG=", "TF_IN_AUTOMATION=1")
				return cmd.CombinedOutput()
			}
			if _, err := run("init", "-input=false"); err != nil {
				t.Fatal("fresh-zone Terraform init failed")
			}
			var zoneID int
			t.Cleanup(func() {
				if _, err := run("destroy", "-input=false", "-auto-approve"); err != nil {
					recovery, err := os.MkdirTemp("", "s3sync-bunny-recovery-")
					if err != nil {
						t.Errorf("test zone %s cleanup failed and private recovery directory could not be created", bucket)
						return
					}
					for _, name := range []string{"terraform.tfstate", "terraform.tfstate.backup", "main.tf.json", ".terraform.lock.hcl"} {
						if data, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
							_ = os.WriteFile(filepath.Join(recovery, name), data, 0600)
						}
					}
					t.Errorf("test zone %s cleanup failed; private recovery state: %s", bucket, recovery)
					return
				}
				if zoneID != 0 {
					request, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("https://api.bunny.net/storagezone/%d", zoneID), nil)
					request.Header.Set("AccessKey", key)
					response, err := (&http.Client{Timeout: 15 * time.Second}).Do(request)
					if err != nil {
						t.Error("cannot independently verify test zone deletion")
						return
					}
					_ = response.Body.Close()
					if response.StatusCode != http.StatusNotFound {
						t.Errorf("deleted test zone GET returned HTTP %d", response.StatusCode)
					}
				}
			})
			start := time.Now()
			if output, err := run("apply", "-input=false", "-auto-approve"); err != nil {
				t.Fatalf("fresh-zone apply failed: %s", strings.ReplaceAll(string(output), key, "[redacted]"))
			}
			t.Logf("zone creation and first upload passed without waiter: %s (%s)", bucket, time.Since(start).Round(time.Millisecond))
			output, err := run("output", "-json")
			if err != nil {
				t.Fatal("cannot read private test connection")
			}
			var connection struct {
				Connection struct {
					Value struct {
						ID       int    `json:"id"`
						Endpoint string `json:"endpoint"`
						Password string `json:"password"`
					} `json:"value"`
				} `json:"connection"`
			}
			if json.Unmarshal(output, &connection) != nil || connection.Connection.Value.Password == "" {
				t.Fatal("missing private connection")
			}
			zoneID = connection.Connection.Value.ID
			client := s3.NewFromConfig(aws.Config{Region: "de", Credentials: credentials.NewStaticCredentialsProvider(bucket, connection.Connection.Value.Password, ""), HTTPClient: awshttp.NewBuildableClient().WithTimeout(20 * time.Second)}, func(options *s3.Options) {
				options.BaseEndpoint = aws.String(connection.Connection.Value.Endpoint)
				options.UsePathStyle = true
				options.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
			})
			object, err := client.GetObject(context.Background(), &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("acceptance/ready.txt")})
			if err != nil {
				t.Fatal("independent fresh-zone body check failed")
			}
			actual, err := io.ReadAll(object.Body)
			_ = object.Body.Close()
			if err != nil || string(actual) != body {
				t.Fatal("fresh-zone body mismatch")
			}
		})
	}
}
