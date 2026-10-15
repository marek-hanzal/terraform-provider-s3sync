Retry short-lived bucket propagation errors after provisioning S3-compatible storage.

- Add the exact S3 `NoSuchBucket` code to the existing AWS SDK retry classification. A newly created Bunny zone can become reachable without failing its first upload.
- Preserve SDK exponential backoff, cancellation, replayable streams and the configured `max_attempts` budget (5 by default).
- Do not retry generic 404/`NotFound`, missing objects or permission failures.
- Prove recovery, retry exhaustion, disabled retries and complete PUT body replay with focused HTTP regressions.
- Add live Terraform acceptance that creates a Bunny S3 zone and uploads its first file in one apply, without an external waiter; independent body and deletion checks verify settlement.
- Build immutable macOS amd64/arm64 and Linux amd64 artifacts locally; no GitHub Actions.

This is a prototype release. Read docs/prototype-validation.md for evidence and
remaining compatibility limits before adopting it in production. Existing
Serakki deployment remains unchanged.
