Prototype bulk S3 synchronization provider, built and released locally.

- One Terraform resource per file set, with explicit SHA-256 source admission and paginated remote comparison.
- SDK-managed signing, bounded retries/timeouts and replayable file streams.
- Actual Terraform lifecycle regression tests for incremental uploads, drift repair, interrupted/failed apply recovery, dependency barriers and safe ownership.
- Separate exclusive-prefix prune operation for cleanup after publication and CDN invalidation.
- Standalone macOS amd64/arm64 and Linux amd64 executables with SHA256SUMS, source/build provenance and a consumer pin manifest.
- No GitHub Actions workflows, Terraform Registry publication or consumer checkout dependency.

This is a prototype release. Read docs/prototype-validation.md for evidence and
remaining compatibility limits before adopting it in production. Existing
Serakki deployment remains unchanged.
