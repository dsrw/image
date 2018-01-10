package layout

import (
	"bytes"
	"io/ioutil"
	"os"
	"testing"

	"path/filepath"

	"github.com/containers/image/types"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readerFromFunc allows implementing Reader by any function, e.g. a closure.
type readerFromFunc func([]byte) (int, error)

func (fn readerFromFunc) Read(p []byte) (int, error) {
	return fn(p)
}

// TestPutBlobDigestFailure simulates behavior on digest verification failure.
func TestPutBlobDigestFailure(t *testing.T) {
	const digestErrorString = "Simulated digest error"
	const blobDigest = "sha256:e692418e4cbaf90ca69d05a66403747baa33ee08806650b51fab815ad7fc331f"

	ref, tmpDir := refToTempOCI(t)
	defer os.RemoveAll(tmpDir)
	dirRef, ok := ref.(ociReference)
	require.True(t, ok)
	blobPath, err := dirRef.blobPath(blobDigest, "")
	assert.NoError(t, err)

	firstRead := true
	reader := readerFromFunc(func(p []byte) (int, error) {
		_, err := os.Lstat(blobPath)
		require.Error(t, err)
		require.True(t, os.IsNotExist(err))
		if firstRead {
			if len(p) > 0 {
				firstRead = false
			}
			for i := 0; i < len(p); i++ {
				p[i] = 0xAA
			}
			return len(p), nil
		}
		return 0, errors.Errorf(digestErrorString)
	})

	dest, err := ref.NewImageDestination(nil)
	require.NoError(t, err)
	defer dest.Close()
	_, err = dest.PutBlob(reader, types.BlobInfo{Digest: blobDigest, Size: -1})
	assert.Error(t, err)
	assert.Contains(t, digestErrorString, err.Error())
	err = dest.Commit()
	assert.NoError(t, err)

	_, err = os.Lstat(blobPath)
	require.Error(t, err)
	require.True(t, os.IsNotExist(err))
}

// TestPutManifestAppendsToExistingManifest tests that new manifests are getting added to existing index.
func TestPutManifestAppendsToExistingManifest(t *testing.T) {
	ref, tmpDir := refToTempOCI(t)
	defer os.RemoveAll(tmpDir)

	ociRef, ok := ref.(ociReference)
	require.True(t, ok)

	// iniitally we have one manifest
	index, err := ociRef.getIndex()
	assert.NoError(t, err)
	assert.Equal(t, 1, len(index.Manifests), "Unexpected number of manifests")

	// create a new test reference
	ociRef2, err := NewReference(tmpDir, "new-image")
	assert.NoError(t, err)

	putTestManifest(t, ociRef2.(ociReference), tmpDir)

	index, err = ociRef.getIndex()
	assert.NoError(t, err)
	assert.Equal(t, 2, len(index.Manifests), "Unexpected number of manifests")
}

// TestPutManifestTwice tests that existing manifest gets updated and not appended.
func TestPutManifestTwice(t *testing.T) {
	ref, tmpDir := refToTempOCI(t)
	defer os.RemoveAll(tmpDir)

	ociRef, ok := ref.(ociReference)
	require.True(t, ok)

	putTestConfig(t, ociRef, tmpDir)
	putTestManifest(t, ociRef, tmpDir)
	putTestManifest(t, ociRef, tmpDir)

	index, err := ociRef.getIndex()
	assert.NoError(t, err)
	assert.Equal(t, 1, len(index.Manifests), "Unexpected number of manifests")
}

func putTestConfig(t *testing.T, ociRef ociReference, tmpDir string) {
	data, err := ioutil.ReadFile("../../image/fixtures/oci1-config.json")
	assert.NoError(t, err)
	imageDest, err := newImageDestination(nil, ociRef)
	assert.NoError(t, err)

	_, err = imageDest.PutBlob(bytes.NewReader(data), types.BlobInfo{Size: int64(len(data)), Digest: digest.FromBytes(data)})
	assert.NoError(t, err)

	err = imageDest.Commit()
	assert.NoError(t, err)

	paths := []string{}
	filepath.Walk(tmpDir, func(path string, info os.FileInfo, err error) error {
		paths = append(paths, path)
		return nil
	})

	digest := digest.FromBytes(data).Hex()
	assert.Contains(t, paths, filepath.Join(tmpDir, "blobs", "sha256", digest), "The OCI directory does not contain the new config data")
}
func putTestManifest(t *testing.T, ociRef ociReference, tmpDir string) {
	data, err := ioutil.ReadFile("../../image/fixtures/oci1.json")
	assert.NoError(t, err)
	imageDest, err := newImageDestination(nil, ociRef)
	assert.NoError(t, err)

	err = imageDest.PutManifest(data, nil)
	assert.NoError(t, err)

	err = imageDest.Commit()
	assert.NoError(t, err)

	paths := []string{}
	filepath.Walk(tmpDir, func(path string, info os.FileInfo, err error) error {
		paths = append(paths, path)
		return nil
	})

	digest := digest.FromBytes(data).Hex()
	assert.Contains(t, paths, filepath.Join(tmpDir, "blobs", "sha256", digest), "The OCI directory does not contain the new manifest data")
}

func putTestIndex(t *testing.T, ociRef ociReference, tmpDir string) {
	data, err := ioutil.ReadFile("../../manifest/fixtures/ociv1.image.index.json")
	assert.NoError(t, err)
	imageDest, err := newImageDestination(nil, ociRef)
	assert.NoError(t, err)

	err = imageDest.PutManifest(data, nil)
	assert.NoError(t, err)

	err = imageDest.Commit()
	assert.NoError(t, err)

	paths := []string{}
	filepath.Walk(tmpDir, func(path string, info os.FileInfo, err error) error {
		paths = append(paths, path)
		return nil
	})

	digest := digest.FromBytes(data).Hex()
	assert.Contains(t, paths, filepath.Join(tmpDir, "blobs", "sha256", digest), "The OCI directory does not contain the new manifest index")
}
