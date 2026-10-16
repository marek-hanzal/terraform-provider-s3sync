Keep publication identity independent of the local checkout directory.

- Add optional provider `source_directory`, an explicit absolute local root for relative `files.source` paths.
- Resolve relative sources only during apply admission; preserve configured relative paths in Terraform state and the manifest digest.
- Reject missing roots, relative traversal, ambiguous path segments and control characters before remote writes. Absolute sources remain supported.
- Preserve full local SHA-256 admission, including acknowledged files that would otherwise be skipped. Plans and refresh remain write-free and do not open local source files.
- Verify the actual Terraform lifecycle: remove the original source root, move the same relative manifest to a second root, obtain a no-change plan and apply without PUT/DELETE or `replace_triggered_by` consumer replacement, then verify changed bytes and checksum failure at the new root.
- Build immutable macOS amd64/arm64 and Linux amd64 artifacts locally; no GitHub Actions.

This is a prototype release. Read docs/prototype-validation.md for evidence and
remaining compatibility limits before adopting it in production.
