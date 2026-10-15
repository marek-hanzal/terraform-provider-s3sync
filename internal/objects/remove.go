package objects

import (
	"context"
	"regexp"
	"sort"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Remove uses the acknowledged inventory rather than a listing, so destroying a
// file-set resource cannot acquire ownership of unrelated objects in its prefix.
func (client *Client) Remove(ctx context.Context, bucket, prefix string, inventory map[string]Record) error {
	if err := ValidateDestination(bucket, prefix, false); err != nil {
		return err
	}
	if err := validateInventory(inventory); err != nil {
		return err
	}
	for _, key := range sortedKeys(inventory) {
		if err := client.removeOne(ctx, bucket, prefix, key); err != nil {
			return err
		}
	}
	return nil
}

// Prune is deliberately separate from Reconcile. Consumers can publish manifests
// and entry points, purge caches, then clean only their declared keys.
func (client *Client) Prune(ctx context.Context, bucket, prefix string, keep []string, keyPattern *regexp.Regexp) (int, error) {
	if err := ValidateDestination(bucket, prefix, true); err != nil {
		return 0, err
	}
	if err := ValidateKeys(keep); err != nil {
		return 0, err
	}
	remote, err := client.List(ctx, bucket, prefix)
	if err != nil {
		return 0, err
	}
	deleted := 0
	for _, key := range PruneKeys(remote, keep, keyPattern) {
		if err := client.removeOne(ctx, bucket, prefix, key); err != nil {
			return deleted, err
		}
		deleted++
	}
	return deleted, nil
}

// PruneKeys is the shared deletion boundary for read-only refresh and cleanup.
// Matching uses keys relative to the prefix; nil preserves match-all ownership.
func PruneKeys(remote map[string]Remote, keep []string, keyPattern *regexp.Regexp) []string {
	retained := make(map[string]struct{}, len(keep))
	for _, key := range keep {
		retained[key] = struct{}{}
	}
	pending := make([]string, 0, len(remote))
	for key := range remote {
		if _, keep := retained[key]; !keep && (keyPattern == nil || keyPattern.MatchString(key)) {
			pending = append(pending, key)
		}
	}
	sort.Strings(pending)
	return pending
}

func (client *Client) removeOne(ctx context.Context, bucket, prefix, key string) error {
	if err := ctx.Err(); err != nil {
		return safeError("delete", key, err)
	}
	_, err := client.api.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(prefix + key),
	})
	if err != nil {
		return safeError("delete", key, err)
	}
	return nil
}
