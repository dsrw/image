package image

import (
	"github.com/containers/image/manifest"
	"github.com/containers/image/types"
	"github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
)

func manifestSchema2FromManifestList(ctx *types.SystemContext, src types.ImageSource, manblob []byte) (genericManifest, error) {
	list, err := manifest.Schema2ListFromManifest(manblob)
	if err != nil {
		return nil, err
	}
	targetManifestDigest, err := list.ChooseDigest(ctx)
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

// ChooseManifestInstanceFromManifestList returns a digest of a manifest appropriate
// for the current system from the manifest available from src.
func ChooseManifestInstanceFromManifestList(ctx *types.SystemContext, src types.UnparsedImage) (digest.Digest, error) {
	blob, mt, err := src.Manifest()
	if err != nil {
		return "", err
	}
	list, err := manifest.ListFromBlob(blob, mt)
	if err != nil {
		return "", err
	}
	return list.ChooseDigest(ctx)
}
