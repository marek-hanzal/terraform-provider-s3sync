package provider_test

import (
	"strings"
	"testing"

	"github.com/marek-hanzal/terraform-provider-s3sync/internal/teststore"
)

// A cleanup filter must constrain both refresh and deletion, or protected files
// either disappear or keep producing non-settling Terraform plans.
func TestTerraformPruneKeyPattern(t *testing.T) {
	binary := buildProvider(t)
	t.Run("matching_keys_only_and_settled_refresh", func(t *testing.T) {
		store := teststore.New(t)
		tf := newTerraform(t, binary, store)
		tf.documents, tf.prune = true, true
		tf.pruneKeyPattern = `^[a-f0-9]{64}\.(avif|ogg)$`
		retained := strings.Repeat("b", 64) + ".ogg"
		obsolete := strings.Repeat("a", 64) + ".avif"
		tf.file(retained, "retained", "audio/ogg")
		preserved := []string{
			"assets/custom.avif",
			"assets/" + strings.Repeat("A", 64) + ".avif",
			"assets/nested/" + obsolete,
			"assets/" + strings.Repeat("c", 64) + ".txt",
			"elsewhere/" + obsolete,
		}
		for _, key := range append(preserved, "assets/"+obsolete) {
			store.Seed(fixtureBucket, key, []byte("existing"), "application/octet-stream")
		}
		tf.writeConfig()
		tf.run(0, "init", "-input=false")
		store.ResetStats()
		tf.plan(2, "cold.plan")
		assertNoWrites(t, store.Stats())
		tf.run(0, "apply", "-input=false", "-auto-approve", "cold.plan")
		if stats := store.Stats(); stats.Delete != 1 || stats.DeleteByKey["assets/"+obsolete] != 1 {
			t.Fatalf("filtered cleanup deleted keys outside its ownership: %+v", stats)
		}
		for _, key := range preserved {
			assertBody(t, store, key, "existing")
		}
		assertBody(t, store, "assets/"+retained, "retained")

		// A matching remote addition must create a cleanup plan; protected remote
		// additions alone must never keep the resource dirty after apply.
		store.Seed(fixtureBucket, "assets/"+obsolete, []byte("new obsolete"), "application/octet-stream")
		store.Seed(fixtureBucket, "assets/operator-note.txt", []byte("preserve"), "text/plain")
		store.ResetStats()
		tf.plan(2, "cleanup.plan")
		assertNoWrites(t, store.Stats())
		tf.run(0, "apply", "-input=false", "-auto-approve", "cleanup.plan")
		if stats := store.Stats(); stats.Put != 0 || stats.Delete != 1 || stats.DeleteByKey["assets/"+obsolete] != 1 {
			t.Fatalf("refresh and apply disagree about cleanup candidates: %+v", stats)
		}
		assertBody(t, store, "assets/operator-note.txt", "preserve")
		store.ResetStats()
		tf.plan(0, "settled.plan")
		assertNoWrites(t, store.Stats())

		// Removing the filter restores existing match-all behavior explicitly.
		tf.pruneKeyPattern = ""
		tf.writeConfig()
		tf.apply()
		if stats := store.Stats(); stats.Delete != 5 {
			t.Fatalf("default cleanup must remove all five non-kept in-prefix keys: %+v", stats)
		}
		assertBody(t, store, "elsewhere/"+obsolete, "existing")
		assertBody(t, store, "assets/"+retained, "retained")
	})
	t.Run("invalid_pattern_rejected_before_writes", func(t *testing.T) {
		store := teststore.New(t)
		tf := newTerraform(t, binary, store)
		tf.documents, tf.prune = true, true
		tf.pruneKeyPattern = "["
		tf.file("desired.bin", "body", "application/octet-stream")
		store.Seed(fixtureBucket, "assets/operator-note.txt", []byte("preserve"), "text/plain")
		tf.writeConfig()
		tf.run(0, "init", "-input=false")
		store.ResetStats()
		output := tf.run(1, "plan", "-input=false")
		if !strings.Contains(output, "Invalid cleanup key pattern") {
			t.Fatalf("invalid filter did not produce a direct diagnostic: %s", output)
		}
		tf.run(1, "apply", "-input=false", "-auto-approve")
		assertNoWrites(t, store.Stats())
		assertBody(t, store, "assets/operator-note.txt", "preserve")
	})
}
