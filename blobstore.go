package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// blobStore holds uploaded CSVs and built catalog databases, the source of
// truth for song data. Keys look like "catalogs/12/v3.csv".
type blobStore interface {
	put(ctx context.Context, key string, r io.Reader) error
	// get returns errBlobNotFound for a missing key.
	get(ctx context.Context, key string) (io.ReadCloser, error)
	// delete and deletePrefix succeed when there is nothing to delete.
	delete(ctx context.Context, key string) error
	deletePrefix(ctx context.Context, prefix string) error // prefix ends in "/"
}

var errBlobNotFound = errors.New("blob not found")

func csvBlobKey(catalogID uint, version int) string {
	return fmt.Sprintf("catalogs/%d/v%d.csv", catalogID, version)
}

func dbBlobKey(catalogID uint, version int) string {
	return fmt.Sprintf("catalogs/%d/v%d.db", catalogID, version)
}

func catalogBlobPrefix(catalogID uint) string { return fmt.Sprintf("catalogs/%d/", catalogID) }

// dirBlobStore keeps blobs as files under a directory, for development and tests.
type dirBlobStore struct{ dir string }

func (s dirBlobStore) path(key string) (string, error) {
	if !fs.ValidPath(key) {
		return "", fmt.Errorf("invalid blob key %q", key)
	}
	return filepath.Join(s.dir, filepath.FromSlash(key)), nil
}

func (s dirBlobStore) put(_ context.Context, key string, r io.Reader) error {
	path, err := s.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeFileAtomic(path, r)
}

func (s dirBlobStore) get(_ context.Context, key string) (io.ReadCloser, error) {
	path, err := s.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errBlobNotFound
	}
	return f, err
}

func (s dirBlobStore) delete(_ context.Context, key string) error {
	path, err := s.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (s dirBlobStore) deletePrefix(_ context.Context, prefix string) error {
	path, err := s.path(strings.TrimSuffix(prefix, "/"))
	if err != nil {
		return err
	}
	return os.RemoveAll(path)
}

// writeFileAtomic writes r to path through a temp file in the same directory,
// so readers never see a partial file.
func writeFileAtomic(path string, r io.Reader) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
