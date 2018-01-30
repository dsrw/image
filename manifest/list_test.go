package manifest

import (
	"io/ioutil"
	"path/filepath"
	"testing"

	"github.com/containers/image/types"
	digest "github.com/opencontainers/go-digest"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func isOCI1Index(i interface{}) bool {
	switch i.(type) {
	case *OCI1Index:
		return true
	}
	return false
}

func isSchema2List(i interface{}) bool {
	switch i.(type) {
	case *Schema2List:
		return true
	}
	return false
}

func cloneOCI1Index(i interface{}) ManifestList {
	if impl, ok := i.(*OCI1Index); ok {
		return OCI1IndexClone(impl)
	}
	return nil
}

func cloneSchema2List(i interface{}) ManifestList {
	if impl, ok := i.(*Schema2List); ok {
		return Schema2ListClone(impl)
	}
	return nil
}

func pare(m ManifestList) {
	if impl, ok := m.(*OCI1Index); ok {
		impl.Annotations = nil
	}
	if impl, ok := m.(*Schema2List); ok {
		for i := range impl.Manifests {
			impl.Manifests[i].Platform.Features = nil
		}
	}
	return
}

func TestParseLists(t *testing.T) {
	cases := []struct {
		path      string
		mimeType  string
		checkType (func(interface{}) bool)
		clone     (func(interface{}) ManifestList)
	}{
		{"ociv1.image.index.json", imgspecv1.MediaTypeImageIndex, isOCI1Index, cloneOCI1Index},
		{"v2list.manifest.json", DockerV2ListMediaType, isSchema2List, cloneSchema2List},
	}
	for _, c := range cases {
		manifest, err := ioutil.ReadFile(filepath.Join("fixtures", c.path))
		require.NoError(t, err, "error reading file %q", filepath.Join("fixtures", c.path))
		assert.Equal(t, GuessMIMEType(manifest), c.mimeType)

		_, err = FromBlob(manifest, c.mimeType)
		require.Error(t, err, "manifest list %q should not parse as single images", c.path)

		m, err := ListFromBlob(manifest, c.mimeType)
		require.NoError(t, err, "manifest list %q  should parse as list types", c.path)
		assert.True(t, c.checkType(m), "manifest %q is not of the expected implementation type", c.path)
		pare(m)

		clone := c.clone(m)
		assert.Equal(t, clone, m, "manifest %q is missing some fields after being cloned", c.path)

		index, err := m.ToOCI1Index()
		require.NoError(t, err, "error converting %q to an OCI1Index", c.path)

		list, err := m.ToSchema2List()
		require.NoError(t, err, "error converting %q to an Schema2List", c.path)

		index2, err := list.ToOCI1Index()
		assert.Equal(t, index, index2, "index %q lost data in conversion", c.path)

		list2, err := index.ToSchema2List()
		assert.Equal(t, list, list2, "list %q lost data in conversion", c.path)
	}
}

func TestChooseDigest(t *testing.T) {
	for _, manifestList := range []struct {
		listFile           string
		rawManifest        []byte
		matchedInstances   map[string]digest.Digest
		unmatchedInstances []string
	}{
		{
			listFile: "schema2list.json",
			matchedInstances: map[string]digest.Digest{
				"amd64": "sha256:030fcb92e1487b18c974784dcc110a93147c9fc402188370fbfd17efabffc6af",
				"s390x": "sha256:e5aa1b0a24620228b75382997a0977f609b3ca3a95533dafdef84c74cc8df642",
				// There are several "arm" images with different variants;
				// the current code returns the first match. NOTE: This is NOT an API promise.
				"arm": "sha256:9142d97ef280a7953cf1a85716de49a24cc1dd62776352afad67e635331ff77a",
			},
			unmatchedInstances: []string{
				"unmatched",
			},
		},
		{
			listFile: "oci1index.json",
			matchedInstances: map[string]digest.Digest{
				"amd64":   "sha256:5b0bcabd1ed22e9fb1310cf6c2dec7cdef19f0ad69efa1f392e94a4333501270",
				"ppc64le": "sha256:e692418e4cbaf90ca69d05a66403747baa33ee08806650b51fab815ad7fc331f",
			},
			unmatchedInstances: []string{
				"unmatched",
			},
		},
	} {
		if len(manifestList.listFile) > 0 {
			man, err := ioutil.ReadFile(filepath.Join("..", "image", "fixtures", manifestList.listFile))
			require.NoError(t, err)
			manifestList.rawManifest = man
		}
		list, err := ListFromBlob(manifestList.rawManifest, GuessMIMEType(manifestList.rawManifest))
		require.NoError(t, err)
		// Match found
		for arch, expected := range manifestList.matchedInstances {
			digest, err := list.ChooseDigest(&types.SystemContext{
				ArchitectureChoice: arch,
				OSChoice:           "linux",
			})
			require.NoError(t, err, arch)
			assert.Equal(t, expected, digest)
		}
		// Not found
		for _, arch := range manifestList.unmatchedInstances {
			_, err := list.ChooseDigest(&types.SystemContext{
				ArchitectureChoice: arch,
				OSChoice:           "linux",
			})
			assert.Error(t, err)
		}
	}
}
