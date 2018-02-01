package manifest

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/containers/image/types"
	digest "github.com/opencontainers/go-digest"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
)

var (
	// SupportedManifestListMIMETypes is a list of the manifest list types that we know how
	// to read/manipulate/write.
	SupportedManifestListMIMETypes = []string{
		DockerV2ListMediaType,
		imgspecv1.MediaTypeImageIndex,
	}
)

// ManifestList is an interface for parsing, modifying lists of image manifests.
// Callers can either use this abstract interface without understanding the details of the formats,
// or instantiate a specific implementation (e.g. manifest.OCI1Index) and access the public members
// directly.
type ManifestList interface {
	// MIMEType returns the MIME type of this particular manifest list.
	MIMEType() string

	// Instances returns a list of the manifests that this list knows of.
	Instances() []types.BlobInfo

	// Update information about the list's instances.  The length of the passed-in slice must
	// match the length of the list of instances which the list already contains, and every field
	// must be specified.
	UpdateInstances([]ManifestListUpdate) error

	// ChooseInstance selects which manifest is most appropriate for the platform described by the
	// SystemContext, or for the current platform if the SystemContext doesn't specify any details.
	ChooseInstance(ctx *types.SystemContext) (digest.Digest, error)

	// ImageID computes a recommended image ID based on the list of images referred to by the manifest.
	ImageID() string

	// Serialize returns the list in a blob format.
	// NOTE: Serialize() does not in general reproduce the original blob if this object was loaded
	// from, even if no modifications were made!
	Serialize() ([]byte, error)

	// ToOCI1Index returns the list rebuilt as an OCI1 index, converting it if necessary.
	ToOCI1Index() (*OCI1Index, error)

	// ToSchema2List returns the list rebuilt as a Schema2 list, converting it if necessary.
	ToSchema2List() (*Schema2List, error)

	// ConvertToMIMEType returns the list rebuilt to the specified MIME type, or an error.
	ConvertToMIMEType(mimeType string) (ManifestList, error)

	// Clone returns a deep copy of this list and its contents.
	Clone() ManifestList
}

// ManifestListUpdate includes the fields which a ManifestList's UpdateInstances() method will modify.
type ManifestListUpdate struct {
	Digest    digest.Digest
	Size      int64
	MediaType string
}

// dupStringSlice returns a deep copy of a slice of strings, or nil if the
// source slice is empty.
func dupStringSlice(list []string) []string {
	if len(list) == 0 {
		return nil
	}
	dup := make([]string, len(list))
	for i := range list {
		dup[i] = list[i]
	}
	return dup
}

// dupStringStringMap returns a deep copy of a map[string]string, or nil if the
// passed-in map is empty or has no contents.
func dupStringStringMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	result := make(map[string]string)
	for k, v := range m {
		result[k] = v
	}
	return result
}

// ListFromBlob parses a list of manifests.
func ListFromBlob(manifest []byte, manifestMIMEType string) (ManifestList, error) {
	normalized := NormalizedMIMEType(manifestMIMEType)
	switch normalized {
	case DockerV2ListMediaType:
		return Schema2ListFromManifest(manifest)
	case imgspecv1.MediaTypeImageIndex:
		return OCI1IndexFromManifest(manifest)
	case DockerV2Schema1MediaType, DockerV2Schema1SignedMediaType, imgspecv1.MediaTypeImageManifest, DockerV2Schema2MediaType:
		return nil, fmt.Errorf("Treating single images as manifest lists is not implemented")
	}
	return nil, fmt.Errorf("Unimplemented manifest MIME type %s (normalized as %s)", manifestMIMEType, normalized)
}

// computeListID computes an image ID using the list of images referred to in a ManifestList.
func computeListID(manifests ManifestList) string {
	instances := manifests.Instances()
	digests := make([][]byte, len(instances))
	for i, manifest := range manifests.Instances() {
		digests[i] = []byte(manifest.Digest.String())
	}
	sort.Slice(digests, func(i, j int) bool { return bytes.Compare(digests[i], digests[j]) < 0 })
	return digest.FromBytes(bytes.Join(digests, []byte{0})).Hex()
}

// ConvertManifestListToMIMEType converts the passed-in manifest list to a manifest
// list of the specified type.
func ConvertManifestListToMIMEType(list ManifestList, manifestMIMEType string) (ManifestList, error) {
	switch normalized := NormalizedMIMEType(manifestMIMEType); normalized {
	case DockerV2ListMediaType:
		return list.ToSchema2List()
	case imgspecv1.MediaTypeImageIndex:
		return list.ToOCI1Index()
	case DockerV2Schema1MediaType, DockerV2Schema1SignedMediaType, imgspecv1.MediaTypeImageManifest, DockerV2Schema2MediaType:
		return nil, fmt.Errorf("Can not convert manifest list to MIME type %q, which is not a list type", manifestMIMEType)
	}
	return nil, fmt.Errorf("Unimplemented manifest MIME type %s", manifestMIMEType)
}
