# Terraform S3 Sync provider

A prototype Terraform Plugin Framework provider for bulk file publication to
S3-compatible storage. One `s3sync_files` resource owns a file manifest rather
than one Terraform resource per object. AWS SDK for Go v2 owns signing, retries,
timeouts and stream replay. No consumer-specific publication logic lives here.

The provider address is `marek-hanzal/s3sync`. Version `0.1.0` is a prototype,
distributed through immutable GitHub release assets and a Terraform filesystem
mirror, without Terraform Registry publication or GitHub Actions.

## File publication

```hcl
terraform {
  required_providers {
    s3sync = { source = "marek-hanzal/s3sync", version = "= 0.1.0" }
  }
}

provider "s3sync" {
  endpoint   = var.s3_endpoint
  region     = var.s3_region
  access_key = var.s3_access_key
  secret_key = var.s3_secret_key
}

resource "s3sync_files" "assets" {
  bucket = var.bucket
  prefix = "app/assets/"
  files = {
    for key in fileset("${path.module}/dist/assets", "**") : key => {
      source = abspath("${path.module}/dist/assets/${key}")
      sha256 = filesha256("${path.module}/dist/assets/${key}")
    }
  }
}
```

Each key is relative to `prefix`. Empty prefixes are allowed for upserts;
nonempty prefixes must end in `/`. Ambiguous keys, control characters,
symlinks and nonregular sources are rejected. Every local SHA-256 is verified
before any remote write, including files that would be skipped. Changed files
between saved plan and apply fail admission. Keep sources stable through apply;
PutObject includes the expected SHA-256 for server-side body validation.

`content_type` defaults to `application/octet-stream`. `concurrency` defaults to 8
and allows 1–64 workers. Admission stops on failure and drains admitted work.
Errors include safe operation/key/code diagnostics rather than raw SDK errors.

Provider `max_attempts` defaults to 5 and `request_timeout_seconds` to 120.
Credentials come only from explicit attributes or `S3SYNC_ACCESS_KEY` and
`S3SYNC_SECRET_KEY`. Sensitive configured credentials remain in protected
Terraform state/plans; media bodies never do. Custom endpoints use path-style
addressing. Omitting `endpoint` uses AWS S3 for the configured region.

## Comparison, refresh and recovery

Desired paths, local hashes and content types are explicit Terraform inputs.
`inventory` stores acknowledged SHA-256/ETag/size/type metadata, including
retired keys while those objects still exist. Paginated ListObjectsV2 checks
remote body identity in bulk; normal unchanged refreshes perform no per-object
HEAD or GET. Missing or changed acknowledged objects invalidate `applied_digest`
and produce a repair plan. Plans and refresh never write.

The default `etag_hash = "opaque"` treats ETags only as server identity tokens.
An unacknowledged object is uploaded once even if a same-sized object exists.
Explicit `etag_hash = "md5"` or `"sha256"` may adopt matching single-part bodies
only when the server guarantees that algorithm. Adoption also HEADs the object
to verify its content type. Multipart/encrypted ETags must not be interpreted
as a body digest. Never infer SHA-256 from a filename.

Successful acknowledgements are preserved in partial state when an apply fails.
Terraform can taint a failed Create and replace the resource on retry; opaque
mode may replay otherwise completed objects in that case. An explicitly verified
hash mode can adopt those completed objects. Same-body metadata-only external
changes are not detected by bulk listing; configured content-type changes do
cause an upload. This prototype does not promise atomic multi-resource releases.

## Explicit cleanup and sequencing

Removed manifest keys are retained. `s3sync_files` destruction retains bodies by
default; `delete_on_destroy = true` deletes only acknowledged inventory keys.
It never acquires deletion ownership of other objects from a listing.

`s3sync_prune` explicitly owns **every file in one exclusive nonempty prefix**:

```hcl
resource "s3sync_prune" "assets" {
  bucket = var.bucket
  prefix = "app/assets/"
  keep   = keys(s3sync_files.assets.files)
  # Put publication/cache-invalidation dependencies here as appropriate.
  depends_on = [s3sync_files.assets]
}
```

An empty `keep` set deletes every file in that prefix during apply. Objects
outside the prefix and directory markers are preserved. A failed deletion stops
cleanup; a later apply lists actual remaining objects and retries. Read reports
obsolete keys in `pending`, while the planned value is empty after cleanup.
Destroying the prune resource does not delete anything.

Use independent file sets and explicit `depends_on` for assets → manifests →
HTML → cache invalidation → prune. CDN invalidation belongs to the CDN provider.
Do not overlap prune prefixes with data owned by other resources or applications.

## Local development and releases

```sh
mise trust
mise install
mise run test
mise run vet
mise run build
```

Permanent tests use an isolated S3 mock and actual Terraform binaries, temporary
state and filesystem mirrors. Optional independent S3 runtime evidence is
described in [prototype validation](docs/prototype-validation.md).

After committing a clean source tree, run `mise run release-build`, then
`mise run release`. Release builds run locally with the pinned Go toolchain,
CGO disabled, read-only modules, trimmed paths, no VCS stamping and a fixed
version. Raw macOS amd64/arm64 and Linux amd64 binaries, `SHA256SUMS`,
`provenance.json` and `provider-manifest.json` are published to this repository.
Published bytes are immutable; no CI or sibling source checkout is required.
Publication creates or resumes a draft, verifies all remote asset bytes and
publishes only the complete release. Rerunning the same clean commit recovers
an interrupted release; divergent tags or existing bytes are rejected.

Download the host asset from the exact release, verify its SHA-256, and rename
it to `terraform-provider-s3sync_v0.1.0` inside
`<mirror>/registry.terraform.io/marek-hanzal/s3sync/0.1.0/<os>_<arch>/`.
Use a task-specific Terraform CLI config with a `filesystem_mirror` for that
address and exclude it from `direct`; never require this source checkout at
consumer runtime. See [release provenance](docs/release-notes.md).
