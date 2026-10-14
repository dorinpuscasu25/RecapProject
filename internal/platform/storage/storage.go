// Package storage abstracts object storage (S3-compatible or local filesystem). Objects are never
// publicly addressable: callers stream them through authenticated endpoints or issue short-lived
// presigned URLs (S3 only).
package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

var ErrNotFound = errors.New("object not found")

type Store interface {
	Put(ctx context.Context, key string, data []byte, contentType string) error
	Get(ctx context.Context, key string) (io.ReadCloser, int64, error)
	Delete(ctx context.Context, key string) error
	// PresignGet returns a short-lived URL, or "" if unsupported (caller streams instead).
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
	Driver() string
}

func validKey(key string) error {
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "..") || strings.ContainsAny(key, "\\\x00") {
		return fmt.Errorf("invalid object key %q", key)
	}
	return nil
}

// Filesystem stores objects under a root directory.
type Filesystem struct{ Root string }

func (f *Filesystem) Driver() string { return "filesystem" }

func (f *Filesystem) path(key string) (string, error) {
	if err := validKey(key); err != nil {
		return "", err
	}
	p := filepath.Join(f.Root, filepath.FromSlash(key))
	rootAbs, _ := filepath.Abs(f.Root)
	pAbs, _ := filepath.Abs(p)
	if !strings.HasPrefix(pAbs, rootAbs+string(filepath.Separator)) {
		return "", fmt.Errorf("object key escapes root")
	}
	return p, nil
}

func (f *Filesystem) Put(_ context.Context, key string, data []byte, _ string) error {
	p, err := f.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err
	}
	tmp := p + ".tmp"
	// #nosec G306 -- 0640 is intended for stored objects
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (f *Filesystem) Get(_ context.Context, key string) (io.ReadCloser, int64, error) {
	p, err := f.path(key)
	if err != nil {
		return nil, 0, err
	}
	fh, err := os.Open(p) // #nosec G304 -- path() confines keys to the storage root
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	st, _ := fh.Stat()
	return fh, st.Size(), nil
}

func (f *Filesystem) Delete(_ context.Context, key string) error {
	p, err := f.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (f *Filesystem) PresignGet(context.Context, string, time.Duration) (string, error) {
	return "", nil
}

// S3 stores objects in an S3-compatible bucket (MinIO locally).
type S3 struct {
	c       *minio.Client
	presign *minio.Client // signs URLs for the public endpoint; same as c when there is none
	bucket  string
}

// NewS3 connects to endpoint. publicEndpoint, when set, is the address clients use (for example
// localhost:8333 while the services reach storage as s3:8333); presigned URLs are signed for it.
func NewS3(ctx context.Context, endpoint, access, secret, bucket, region string, useSSL bool, publicEndpoint string) (*S3, error) {
	opts := &minio.Options{Creds: credentials.NewStaticV4(access, secret, ""), Secure: useSSL, Region: region}
	c, err := minio.New(endpoint, opts)
	if err != nil {
		return nil, err
	}
	presign := c
	if publicEndpoint != "" && publicEndpoint != endpoint {
		// Presigning is computed locally (the region is fixed), so this client never dials.
		if presign, err = minio.New(publicEndpoint, opts); err != nil {
			return nil, fmt.Errorf("S3_PUBLIC_ENDPOINT: %w", err)
		}
	}
	ok, err := c.BucketExists(ctx, bucket)
	if err != nil {
		return nil, fmt.Errorf("check bucket: %w", err)
	}
	if !ok {
		if err := c.MakeBucket(ctx, bucket, minio.MakeBucketOptions{Region: region}); err != nil {
			return nil, fmt.Errorf("create bucket: %w", err)
		}
	}
	return &S3{c: c, presign: presign, bucket: bucket}, nil
}

func (s *S3) Driver() string { return "s3" }

func (s *S3) Put(ctx context.Context, key string, data []byte, contentType string) error {
	if err := validKey(key); err != nil {
		return err
	}
	_, err := s.c.PutObject(ctx, s.bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{ContentType: contentType})
	return err
}

func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	obj, err := s.c.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, 0, err
	}
	st, err := obj.Stat()
	if err != nil {
		obj.Close()
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil, 0, ErrNotFound
		}
		return nil, 0, err
	}
	return obj, st.Size, nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	return s.c.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}

func (s *S3) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	u, err := s.presign.PresignedGetObject(ctx, s.bucket, key, ttl, url.Values{})
	if err != nil {
		return "", err
	}
	return u.String(), nil
}
