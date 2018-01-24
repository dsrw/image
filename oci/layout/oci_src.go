package layout

import (
	"context"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"strconv"

	"github.com/containers/image/pkg/tlsclientconfig"
	"github.com/containers/image/types"
	"github.com/docker/go-connections/tlsconfig"
	"github.com/opencontainers/go-digest"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

type ociImageSource struct {
	ref           ociReference
	index         *imgspecv1.Index
	descriptor    imgspecv1.Descriptor
	client        *http.Client
	sharedBlobDir string
}

// newImageSource returns an ImageSource for reading from an existing directory.
func newImageSource(ctx *types.SystemContext, ref ociReference) (types.ImageSource, error) {
	tr := tlsclientconfig.NewTransport()
	tr.TLSClientConfig = tlsconfig.ServerDefault()

	if ctx != nil && ctx.OCICertPath != "" {
		if err := tlsclientconfig.SetupCertificates(ctx.OCICertPath, tr.TLSClientConfig); err != nil {
			return nil, err
		}
		tr.TLSClientConfig.InsecureSkipVerify = ctx.OCIInsecureSkipTLSVerify
	}

	client := &http.Client{}
	client.Transport = tr
	descriptor, err := ref.getManifestDescriptor()
	if err != nil {
		return nil, err
	}
	index, err := ref.getIndex()
	if err != nil {
		return nil, err
	}
	d := &ociImageSource{ref: ref, index: index, descriptor: descriptor, client: client}
	if ctx != nil {
		// TODO(jonboulle): check dir existence?
		d.sharedBlobDir = ctx.OCISharedBlobDirPath
	}
	return d, nil
}

// Reference returns the reference used to set up this source.
func (s *ociImageSource) Reference() types.ImageReference {
	return s.ref
}

// Close removes resources associated with an initialized ImageSource, if any.
func (s *ociImageSource) Close() error {
	return nil
}

// GetManifest returns the image's manifest along with its MIME type (which may be empty when it can't be determined but the manifest is available).
// It may use a remote (= slow) service.
// If instanceDigest is not nil, it contains a digest of the specific manifest instance to retrieve (when the primary manifest is a manifest list);
// this never happens if the primary manifest is not a manifest list (e.g. if the source never returns manifest lists).
func (s *ociImageSource) GetManifest(instanceDigest *digest.Digest) ([]byte, string, error) {
	var dig digest.Digest
	var mimeType string
	if instanceDigest == nil {
		dig = digest.Digest(s.descriptor.Digest)
		mimeType = s.descriptor.MediaType
	} else {
		dig = *instanceDigest
		for _, md := range s.index.Manifests {
			if md.Digest == dig {
				mimeType = md.MediaType
				break
			}
		}
		if mimeType == "" {
			return nil, "", errors.Errorf("no manifest matching digest %q found in index", dig)
		}
	}

	manifestPath, err := s.ref.blobPath(dig, s.sharedBlobDir)
	if err != nil {
		return nil, "", err
	}
	m, err := ioutil.ReadFile(manifestPath)
	if err != nil {
		return nil, "", err
	}

	return m, mimeType, nil
}

// GetBlob returns a stream for the specified blob, and the blob's size.
func (s *ociImageSource) GetBlob(info types.BlobInfo) (io.ReadCloser, int64, error) {
	if len(info.URLs) != 0 {
		return s.getExternalBlob(info.URLs)
	}

	path, err := s.ref.blobPath(info.Digest, s.sharedBlobDir)
	if err != nil {
		return nil, 0, err
	}

	r, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	fi, err := r.Stat()
	if err != nil {
		return nil, 0, err
	}
	return r, fi.Size(), nil
}

// GetSignatures returns the image's signatures.  It may use a remote (= slow) service.
// If instanceDigest is not nil, it contains a digest of the specific manifest instance to retrieve signatures for
// (when the primary manifest is a manifest list); this never happens if the primary manifest is not a manifest list
// (e.g. if the source never returns manifest lists).
func (s *ociImageSource) GetSignatures(ctx context.Context, instanceDigest *digest.Digest) ([][]byte, error) {
	return [][]byte{}, nil
}

func (s *ociImageSource) getExternalBlob(urls []string) (io.ReadCloser, int64, error) {
	errWrap := errors.New("failed fetching external blob from all urls")
	for _, url := range urls {
		resp, err := s.client.Get(url)
		if err != nil {
			errWrap = errors.Wrapf(errWrap, "fetching %s failed %s", url, err.Error())
			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			errWrap = errors.Wrapf(errWrap, "fetching %s failed, response code not 200", url)
			continue
		}

		return resp.Body, getBlobSize(resp), nil
	}

	return nil, 0, errWrap
}

// LayerInfosForCopy returns either nil (meaning the values in the manifest are fine), or updated values for the layer
// blobsums that are listed in the image's manifest.  If values are returned, they should be used when using GetBlob()
// to read the image's layers.
// If instanceDigest is not nil, it contains a digest of the specific manifest instance to retrieve BlobInfos for
// (when the primary manifest is a manifest list); this never happens if the primary manifest is not a manifest list
// (e.g. if the source never returns manifest lists).
// The Digest field is guaranteed to be provided; Size may be -1.
// WARNING: The list may contain duplicates, and they are semantically relevant.
func (s *ociImageSource) LayerInfosForCopy(*digest.Digest) []types.BlobInfo {
	return nil
}

func getBlobSize(resp *http.Response) int64 {
	size, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	if err != nil {
		size = -1
	}
	return size
}
