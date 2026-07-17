package backups

import (
	"context"
	"fmt"
	"io"
	"time"
)

// RunVolume performs one volume backup: a tar stream (the daemon builds it — see
// volumes.Snapshot) through the same pipe as a database dump:
//
//	tar → gzip → age (optional) → S3 multipart upload
//
// Same constant memory, same fail-loudly teardown, same invariant with no exceptions:
// encryption is to PUBLIC age recipients only, so the box cannot read its own backups.
// A volume backup holds someone's data — the exact material the invariant exists for.
//
// The caller owns the snapshot reader and closes it; consistency (stopping consumers of a
// live volume) is also the caller's, decided per job, in writing.
func RunVolume(ctx context.Context, snapshot io.Reader, volumeName string, dst Destination, now time.Time) (*Result, error) {
	client, err := newS3(ctx, dst)
	if err != nil {
		return nil, err
	}

	key := objectKey(dst.Prefix, "volume-"+volumeName, Extension(Volume, dst.Encrypt), now)

	pr, pw := io.Pipe()
	go func() {
		err := compressAndEncrypt(pw, snapshot, dst)
		// Closing the pipe with the error is what makes the uploader fail rather than
		// cheerfully finish a truncated object — a torn tar in the bucket looks exactly
		// like a backup until the day it is needed.
		_ = pw.CloseWithError(err)
	}()

	n, err := upload(ctx, client, dst.Bucket, key, pr)
	if err != nil {
		_ = pr.CloseWithError(err)
		return nil, err
	}

	if n == 0 {
		_ = client.RemoveObject(context.WithoutCancel(ctx), dst.Bucket, key, removeOpts())
		return nil, fmt.Errorf("backups: the snapshot produced no data")
	}
	return &Result{ObjectKey: key, Bytes: n}, nil
}
