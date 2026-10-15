# S3 Sync provider

Keep this provider independent of its consumers. Go and Terraform Plugin Framework own implementation; AWS SDK for Go v2 owns S3 transport and retries. No application-specific publication logic, shell upload commands, or GitHub Actions workflows.

Use the pinned mise toolchain. Run `mise run test`, `mise run vet`, and local release tasks. Builds and releases run locally; immutable GitHub releases distribute standalone binaries, SHA256SUMS and provenance. Never replace published version bytes.

Tests must prove named synchronization, ownership, failure or Terraform lifecycle risks. Keep plan/Read free of writes. Preserve unrelated bucket contents and reject ambiguous keys/prefixes. Report local mock, independent S3 runtime and live Bunny evidence separately.

For substantial changes use bounded workers when useful and at least one standalone read-only reviewer. A worker does not approve its own implementation. Keep ownership explicit when agents share this checkout.
