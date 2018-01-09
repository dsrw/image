package image

import (
	"bytes"
	"io/ioutil"
	"path/filepath"
	"testing"

	"github.com/containers/image/types"
	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChooseDigestFromImageIndex(t *testing.T) {
	manifest, err := ioutil.ReadFile(filepath.Join("fixtures", "oci1index.json"))
	require.NoError(t, err)

	// Match found
	for arch, expected := range map[string]digest.Digest{
		"amd64":   "sha256:5b0bcabd1ed22e9fb1310cf6c2dec7cdef19f0ad69efa1f392e94a4333501270",
		"ppc64le": "sha256:e692418e4cbaf90ca69d05a66403747baa33ee08806650b51fab815ad7fc331f",
	} {
		digest, err := chooseDigestFromImageIndex(&types.SystemContext{
			ArchitectureChoice: arch,
			OSChoice:           "linux",
		}, manifest)
		require.NoError(t, err, arch)
		assert.Equal(t, expected, digest)
	}

	// Invalid manifest list
	_, err = chooseDigestFromImageIndex(&types.SystemContext{
		ArchitectureChoice: "amd64", OSChoice: "linux",
	}, bytes.Join([][]byte{manifest, []byte("!INVALID")}, nil))
	assert.Error(t, err)

	// Not found
	_, err = chooseDigestFromImageIndex(&types.SystemContext{OSChoice: "Unmatched"}, manifest)
	assert.Error(t, err)
}
