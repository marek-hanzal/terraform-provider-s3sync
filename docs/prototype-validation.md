# Prototype validation

Validated locally on macOS arm64 with Go 1.27.2 and Terraform 1.16.5.
The prototype has no GitHub Actions workflows. Existing Serakki deployment has
not been switched to this provider.

## Executable evidence

`mise run test` runs Go race checks, actual Terraform subprocess lifecycles
against an isolated S3 HTTP fixture, and release interruption regressions.
`mise run vet` checks the complete Go module.

| Risk | Proven behavior |
| --- | --- |
| Cold upload and incremental publication | Initial files upload; unchanged apply performs zero PUT/DELETE/HEAD; a same-sized local body change performs one PUT. |
| Remote refresh | ListObjectsV2 pagination crosses 1,000 objects; missing or changed bodies produce a repair plan. Plan/refresh performs no writes. |
| Saved-plan source mutation | Every local SHA-256 is checked before remote writes, including acknowledged files; mutated inputs fail admission. |
| Network failure | SDK retries 503 and lost connections with complete replayed bodies; exhausted retries stop new admission and drain already admitted work. |
| Recovery | Failed Update persists successful acknowledgements, including opaque ETags; the next apply uploads only outstanding files. |
| Terraform sequencing | Asset failure blocks dependent documents and cleanup; document failure blocks cleanup; recovery completes publication before pruning. |
| Cancellation | Terraform SIGINT cancels an in-flight request and prevents dependent document publication. |
| Object ownership | Removed inputs retain bodies; default destroy retains bodies; opted-in destroy deletes only acknowledged keys. Exclusive-prefix prune preserves outside keys and directory markers. |
| Existing object adoption | Explicit MD5/SHA-256 modes validate body identity and content type; default opaque mode never guesses an ETag hash algorithm. |
| Diagnostics | Arbitrary SDK/server messages, signed URLs and configured credentials do not escape through failure diagnostics. |
| Release interruption | Partial uploads remain draft; retry reuses accepted bytes; divergent tags/assets are rejected; only a complete verified release becomes public. |

## Independent S3 runtime

The opt-in `TestTerraformIndependentS3Runtime` passed against an independently
implemented SeaweedFS 4.48 S3 server in a local Docker container. It runs the
actual provider through Terraform with opaque ETags, creates an isolated bucket,
checks uploaded bodies with a separate AWS SDK client, verifies unchanged apply,
same-sized diff, remote drift repair, explicit prune, unrelated-key preservation,
and default destroy retention, then removes its test bucket.

To rerun against an **expendable local S3 test server**, provide explicit test
credentials and permission to create/delete a bucket:

```sh
S3SYNC_TEST_ENDPOINT=http://127.0.0.1:8333 \
S3SYNC_TEST_ACCESS_KEY=local-test-access \
S3SYNC_TEST_SECRET_KEY=local-test-secret \
mise exec -- go test -race -count=1 -v ./internal/provider \
  -run '^TestTerraformIndependentS3Runtime$'
```

This test requires CreateBucket/DeleteBucket and is not a Bunny Storage test.

Set `S3SYNC_TEST_BULK=1` to expand the workload to 1,660 asset files and
518.125 MiB plus one HTML document. Set `S3SYNC_TEST_PROVIDER_BINARY` to the
absolute path of a downloaded release binary to run Terraform lifecycle tests
against the distributed executable instead of building from source.

The expanded workload passed: 1,660 assets / 543,293,450 bytes, plus HTML.
The complete local scenario took 60.56 seconds, including cold publication,
unchanged apply, diff, drift repair, prune, destroy and test-bucket cleanup.
This is local integration evidence, not a Bunny/WAN throughput benchmark.

## Compatibility limits and adoption gate

No live Bunny or AWS account writes have been performed. Local S3 compatibility
does not establish Bunny's signing region, credentials, ETag algorithm, checksum
handling or production rate limits. Before switching Serakki, run an isolated
live Bunny-prefix acceptance using the released binary: cold upload, no-op,
one-file diff, drift repair, interrupted apply recovery and explicit cleanup.
Use opaque ETags until the service's single-part hash contract is verified.

The prototype uses single-part PutObject with explicit SHA-256, bounded workers
and SDK retries. It does not support multipart uploads, temporary session
credentials, ACLs, custom metadata, cache-control headers or batch DeleteObjects.
Bulk ListObjectsV2 cannot detect metadata-only external changes that leave ETag
and size unchanged. Desired content-type changes do cause an upload.

Failed initial Create can be tainted by Terraform. Replacement in opaque mode
may replay accepted files; explicit verified hash modes can re-adopt them.
This is resumable publication with explicit dependency barriers, not an atomic
transaction spanning file sets, CDN invalidation and cleanup. CDN invalidation
remains the CDN provider's responsibility.
