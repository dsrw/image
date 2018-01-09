package image

import (
	"encoding/json"
	"fmt"
	"runtime"

	"github.com/containers/image/manifest"
	"github.com/containers/image/types"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
)

// chooseDigestFromImageIndex parses blob as an OCI1 image index,
// and returns the digest of the image appropriate for the current environment.
func chooseDigestFromImageIndex(ctx *types.SystemContext, blob []byte) (digest.Digest, error) {
	wantedArch := runtime.GOARCH
	if ctx != nil && ctx.ArchitectureChoice != "" {
		wantedArch = ctx.ArchitectureChoice
	}
	wantedOS := runtime.GOOS
	if ctx != nil && ctx.OSChoice != "" {
		wantedOS = ctx.OSChoice
	}

	index := manifest.OCI1Index{}
	if err := json.Unmarshal(blob, &index); err != nil {
		return "", err
	}
	for _, d := range index.Manifests {
		if d.Platform != nil && d.Platform.Architecture == wantedArch && d.Platform.OS == wantedOS {
			return d.Digest, nil
		}
	}
	return "", fmt.Errorf("no image found in image index for architecture %s, OS %s", wantedArch, wantedOS)
}

func manifestOCI1FromImageIndex(ctx *types.SystemContext, src types.ImageSource, manblob []byte) (genericManifest, error) {
	targetManifestDigest, err := chooseDigestFromImageIndex(ctx, manblob)
	if err != nil {
		return nil, err
	}
	manblob, mt, err := src.GetManifest(&targetManifestDigest)
	if err != nil {
		return nil, err
	}

	matches, err := manifest.MatchesDigest(manblob, targetManifestDigest)
	if err != nil {
		return nil, errors.Wrap(err, "Error computing manifest digest")
	}
	if !matches {
		return nil, errors.Errorf("Manifest image does not match selected manifest digest %s", targetManifestDigest)
	}

	return manifestInstanceFromBlob(ctx, src, manblob, mt)
}
