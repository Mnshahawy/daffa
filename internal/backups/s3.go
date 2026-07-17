package backups

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Any S3-compatible store works — Cloudflare R2, Backblaze B2, MinIO, AWS itself. Daffa
// has no opinion about which, and no cloud provider's SDK in its dependency tree beyond
// the one that speaks the protocol.
func newS3(ctx context.Context, d Destination) (*minio.Client, error) {
	endpoint, secure, err := parseEndpoint(d.Endpoint)
	if err != nil {
		return nil, err
	}

	region := d.Region
	if region == "" {
		region = "auto" // what R2 wants; harmless elsewhere
	}

	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(d.KeyID, d.Secret, ""),
		Secure: secure,
		Region: region,
	})
	if err != nil {
		return nil, fmt.Errorf("backups: connecting to %s: %w", d.Endpoint, err)
	}
	return client, nil
}

func parseEndpoint(raw string) (host string, secure bool, err error) {
	if raw == "" {
		return "", false, fmt.Errorf("backups: no S3 endpoint configured")
	}
	if !strings.Contains(raw, "://") {
		// A bare host means https, which is the only sane default for something
		// carrying database dumps across a network.
		return raw, true, nil
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", false, fmt.Errorf("backups: %q is not a valid endpoint: %w", raw, err)
	}
	return u.Host, u.Scheme != "http", nil
}

// upload streams the object up with an unknown length, which is what makes the whole
// constant-memory pipeline possible: minio switches to multipart and never needs to know
// the size in advance, so we never have to buffer the dump to measure it.
func upload(ctx context.Context, client *minio.Client, bucket, key string, r io.Reader) (int64, error) {
	info, err := client.PutObject(ctx, bucket, key, r, -1, minio.PutObjectOptions{
		ContentType: "application/octet-stream",
	})
	if err != nil {
		return 0, fmt.Errorf("backups: uploading to %s/%s: %w", bucket, key, err)
	}
	return info.Size, nil
}

func removeOpts() minio.RemoveObjectOptions { return minio.RemoveObjectOptions{} }

// Snapshot is one stored backup.
type Snapshot struct {
	Key       string    `json:"key"`
	Size      int64     `json:"size"`
	Modified  time.Time `json:"modified"`
	Encrypted bool      `json:"encrypted"`
}

// List returns the snapshots in the bucket, newest first. This is also the verification
// story: if today's snapshot is missing, or suspiciously small, it shows here.
func List(ctx context.Context, d Destination, limit int) ([]Snapshot, error) {
	client, err := newS3(ctx, d)
	if err != nil {
		return nil, err
	}

	prefix := strings.Trim(d.Prefix, "/")
	if prefix != "" {
		prefix += "/"
	}

	var out []Snapshot
	for obj := range client.ListObjects(ctx, d.Bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	}) {
		if obj.Err != nil {
			return nil, fmt.Errorf("backups: listing %s: %w", d.Bucket, obj.Err)
		}
		out = append(out, Snapshot{
			Key:       obj.Key,
			Size:      obj.Size,
			Modified:  obj.LastModified,
			Encrypted: strings.HasSuffix(obj.Key, ".age"),
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Fetch opens a snapshot for reading. The caller closes it.
func Fetch(ctx context.Context, d Destination, key string) (io.ReadCloser, error) {
	client, err := newS3(ctx, d)
	if err != nil {
		return nil, err
	}

	obj, err := client.GetObject(ctx, d.Bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("backups: fetching %s: %w", key, err)
	}
	// GetObject is lazy: it does not talk to the server until first read, so a missing
	// object would otherwise surface as a confusing read error much later.
	if _, err := obj.Stat(); err != nil {
		obj.Close()
		return nil, fmt.Errorf("backups: %s is not in the bucket: %w", key, err)
	}
	return obj, nil
}

// CheckDestination proves the credentials work and the bucket is reachable BEFORE a job
// is saved — so a typo in a secret key is caught by the person typing it, not by a
// scheduled backup failing quietly at 3am.
func CheckDestination(ctx context.Context, d Destination) error {
	client, err := newS3(ctx, d)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	// Listing one object is enough to exercise auth, the endpoint, and the bucket name,
	// without needing permission to create anything.
	for obj := range client.ListObjects(ctx, d.Bucket, minio.ListObjectsOptions{MaxKeys: 1}) {
		if obj.Err != nil {
			return fmt.Errorf("backups: cannot reach %s/%s: %w", d.Endpoint, d.Bucket, obj.Err)
		}
	}
	return nil
}
