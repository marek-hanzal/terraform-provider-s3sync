package objects

import (
	"context"

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
// and entry points, purge caches, then run this exclusive-prefix operation.
func (client *Client) Prune(ctx context.Context, bucket, prefix string, keep []string) (int, error) {
	if err := ValidateDestination(bucket, prefix, true); err != nil {
		return 0, err
	}
	retained := make(map[string]struct{}, len(keep))
	if err := ValidateKeys(keep); err != nil {
		return 0, err
	}
	for _, key := range keep {
		retained[key] = struct{}{}
	}
	remote, err := client.List(ctx, bucket, prefix)
	if err != nil {
		return 0, err
	}
	deleted := 0
	for _, key := range sortedKeys(remote) {
		if _, keep := retained[key]; keep {
			continue
		}
		if err := client.removeOne(ctx, bucket, prefix, key); err != nil {
			return deleted, err
		}
		deleted++
	}
	return deleted, nil
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
