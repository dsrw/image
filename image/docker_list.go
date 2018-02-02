package image

import (
	"github.com/containers/image/manifest"
	"github.com/containers/image/types"
	"github.com/pkg/errors"
)

func manifestSchema2FromList(ctx *types.SystemContext, src types.ImageSource, manblob []byte) (genericManifest, error) {
	list, err := manifest.Schema2ListFromManifest(manblob)
	if err != nil {
		return nil, err
	}
	targetManifestDigest, err := list.ChooseInstance(ctx)
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
