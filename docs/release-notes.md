Constrain explicit cleanup to a declared object-key pattern.

- Add optional `s3sync_prune.key_pattern`, a Go regular expression matched against keys relative to the cleanup prefix.
- Preserve nonmatching files and retained keys; omitted patterns keep existing match-all behavior.
- Use the same candidate selection for read-only refresh and deletion so protected files do not produce recurring cleanup plans.
- Reject invalid patterns during configuration and planning before any writes.
- Verify anchored root-key filtering, nested and arbitrary-file preservation, remote cleanup drift, settled plans and default ownership with actual Terraform lifecycle regressions.
- Build immutable macOS amd64/arm64 and Linux amd64 artifacts locally; no GitHub Actions.

This is a prototype release. Read docs/prototype-validation.md for evidence and
remaining compatibility limits before adopting it in production.
