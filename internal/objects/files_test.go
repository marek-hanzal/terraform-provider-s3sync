package objects

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestAmbiguousKeysAndDestructiveRootPrefixAreRejected(t *testing.T) {
	for _, key := range []string{"", "/absolute", "a\\b", "a//b", "a/../b", "a/./b", "a/", "line\nbreak"} {
		if err := ValidateKeys([]string{key}); err == nil {
			t.Errorf("accepted ambiguous key %q", key)
		}
	}
	for _, prefix := range []string{"assets", "/assets/", "a//", "a/../"} {
		if err := ValidateDestination("bucket", prefix, false); err == nil {
			t.Errorf("accepted ambiguous prefix %q", prefix)
		}
	}
	if err := ValidateDestination("bucket", "", true); err == nil {
		t.Fatal("prune accepted the bucket root")
	}
	if err := ValidateDestination("bucket", "", false); err != nil {
		t.Fatal(err)
	}
}

func TestPlanValidationDoesNotRequireGeneratedSourceAndRejectsHeaderInjection(t *testing.T) {
	file := File{Source: filepath.Join(t.TempDir(), "generated-later"), SHA256: strings.Repeat("a", 64)}
	if err := ValidateFiles(map[string]File{"asset": file}); err != nil {
		t.Fatalf("planning required a source file: %v", err)
	}
	uppercase := file
	uppercase.SHA256 = strings.ToUpper(file.SHA256)
	if err := ValidateFiles(map[string]File{"asset": uppercase}); err == nil {
		t.Fatal("accepted a checksum whose casing would cause perpetual drift")
	}
	file.ContentType = "text/plain\r\nAuthorization: secret"
	if err := ValidateFiles(map[string]File{"asset": file}); err == nil {
		t.Fatal("accepted injected content-type header")
	}
}

func TestDigestTracksEveryInputWithoutMapOrderOrImplicitTypeDifferences(t *testing.T) {
	file := File{Source: "/source", SHA256: strings.Repeat("a", 64)}
	implicit := map[string]File{"b": file, "a": file}
	explicitFile := file
	explicitFile.ContentType = "application/octet-stream"
	explicit := map[string]File{"a": explicitFile, "b": explicitFile}
	if Digest(implicit) != Digest(explicit) {
		t.Fatal("map order or default content type changed the digest")
	}
	for _, changed := range []File{
		{Source: "/another", SHA256: file.SHA256},
		{Source: file.Source, SHA256: strings.Repeat("b", 64)},
		{Source: file.Source, SHA256: file.SHA256, ContentType: "text/plain"},
	} {
		if Digest(map[string]File{"asset": file}) == Digest(map[string]File{"asset": changed}) {
			t.Fatal("a desired input changed without changing the digest")
		}
	}
	if Digest(map[string]File{"asset": file}) == Digest(map[string]File{"another": file}) {
		t.Fatal("object key changed without changing the digest")
	}
}
