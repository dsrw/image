package copy

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"reflect"
	"runtime"
	"strings"
	"time"

	"github.com/containers/image/image"
	"github.com/containers/image/manifest"
	"github.com/containers/image/pkg/compression"
	"github.com/containers/image/signature"
	"github.com/containers/image/transports"
	"github.com/containers/image/types"
	"github.com/opencontainers/go-digest"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	pb "gopkg.in/cheggaaa/pb.v1"
)

type digestingReader struct {
	source           io.Reader
	digester         digest.Digester
	expectedDigest   digest.Digest
	validationFailed bool
}

// newDigestingReader returns an io.Reader implementation with contents of source, which will eventually return a non-EOF error
// and set validationFailed to true if the source stream does not match expectedDigest.
func newDigestingReader(source io.Reader, expectedDigest digest.Digest) (*digestingReader, error) {
	if err := expectedDigest.Validate(); err != nil {
		return nil, errors.Errorf("Invalid digest specification %s", expectedDigest)
	}
	digestAlgorithm := expectedDigest.Algorithm()
	if !digestAlgorithm.Available() {
		return nil, errors.Errorf("Invalid digest specification %s: unsupported digest algorithm %s", expectedDigest, digestAlgorithm)
	}
	return &digestingReader{
		source:           source,
		digester:         digestAlgorithm.Digester(),
		expectedDigest:   expectedDigest,
		validationFailed: false,
	}, nil
}

func (d *digestingReader) Read(p []byte) (int, error) {
	n, err := d.source.Read(p)
	if n > 0 {
		if n2, err := d.digester.Hash().Write(p[:n]); n2 != n || err != nil {
			// Coverage: This should not happen, the hash.Hash interface requires
			// d.digest.Write to never return an error, and the io.Writer interface
			// requires n2 == len(input) if no error is returned.
			return 0, errors.Wrapf(err, "Error updating digest during verification: %d vs. %d", n2, n)
		}
	}
	if err == io.EOF {
		actualDigest := d.digester.Digest()
		if actualDigest != d.expectedDigest {
			d.validationFailed = true
			return 0, errors.Errorf("Digest did not match, expected %s, got %s", d.expectedDigest, actualDigest)
		}
	}
	return n, err
}

// copier allows us to keep track of diffID values for blobs, and other
// data shared across one or more images in a possible manifest list.
type copier struct {
	copiedBlobs      map[digest.Digest]digest.Digest
	cachedDiffIDs    map[digest.Digest]digest.Digest
	dest             types.ImageDestination
	rawSource        types.ImageSource
	reportWriter     io.Writer
	progressInterval time.Duration
	progress         chan types.ProgressProperties
}

// imageCopier tracks state specific to a single image (possibly an item of a manifest list)
type imageCopier struct {
	c                 *copier
	manifestUpdates   *types.ManifestUpdateOptions
	src               types.Image
	diffIDsAreNeeded  bool
	canModifyManifest bool
}

const (
	// CopyOnlyCurrentRuntimeImage is a value which, when set in
	// Options.MultipleImages, indicates that the caller expects only one
	// image to be copied, so if the source reference refers to a list of
	// images, one that matches the current system will be selected.
	CopyOnlyCurrentRuntimeImage = iota
	// CopyAllImages is a value which, when set in Options.MultipleImages,
	// indicates that the caller expects to copy multiple images, and if the
	// source reference refers to a list of images, but the target reference
	// can only accept one image, an error should be returned.
	CopyAllImages
)

// Options allows supplying non-default configuration modifying the behavior of CopyImage.
type Options struct {
	RemoveSignatures bool   // Remove any pre-existing signatures. SignBy will still add a new signature.
	SignBy           string // If non-empty, asks for a signature to be added during the copy, and specifies a key ID, as accepted by signature.NewGPGSigningMechanism().SignDockerManifest(),
	ReportWriter     io.Writer
	SourceCtx        *types.SystemContext
	DestinationCtx   *types.SystemContext
	ProgressInterval time.Duration                 // time to wait between reports to signal the progress channel
	Progress         chan types.ProgressProperties // Reported to when ProgressInterval has arrived for a single artifact+offset.
	// manifest MIME type of image set by user. "" is default and means use the autodetection to the the manifest MIME type
	ForceManifestMIMEType string
	MultipleImages        int // set to either CopyOnlyCurrentRuntimeImage or CopyAllImages
}

// Image copies image from srcRef to destRef, using policyContext to validate
// source image admissibility.
func Image(policyContext *signature.PolicyContext, destRef, srcRef types.ImageReference, options *Options) (retErr error) {
	// NOTE this function uses an output parameter for the error return value.
	// Setting this and returning is the ideal way to return an error.
	//
	// the defers in this routine will wrap the error return with its own errors
	// which can be valuable context in the middle of a multi-streamed copy.
	if options == nil {
		options = &Options{}
	}

	if options.MultipleImages != CopyAllImages && options.MultipleImages != CopyOnlyCurrentRuntimeImage {
		return errors.Errorf("Invalid value for options.MultipleImages: %d", options.MultipleImages)
	}
	reportWriter := ioutil.Discard

	if options.ReportWriter != nil {
		reportWriter = options.ReportWriter
	}

	dest, err := destRef.NewImageDestination(options.DestinationCtx)
	if err != nil {
		return errors.Wrapf(err, "Error initializing destination %s", transports.ImageName(destRef))
	}
	defer func() {
		if err := dest.Close(); err != nil {
			retErr = errors.Wrapf(retErr, " (dest: %v)", err)
		}
	}()

	rawSource, err := srcRef.NewImageSource(options.SourceCtx)
	if err != nil {
		return errors.Wrapf(err, "Error initializing source %s", transports.ImageName(srcRef))
	}
	defer func() {
		if err := rawSource.Close(); err != nil {
			retErr = errors.Wrapf(retErr, " (src: %v)", err)
		}
	}()

	c := &copier{
		copiedBlobs:      make(map[digest.Digest]digest.Digest),
		cachedDiffIDs:    make(map[digest.Digest]digest.Digest),
		dest:             dest,
		rawSource:        rawSource,
		reportWriter:     reportWriter,
		progressInterval: options.ProgressInterval,
		progress:         options.Progress,
	}

	unparsedToplevel := image.UnparsedInstance(rawSource, nil)
	mfest, manifestType, err := unparsedToplevel.Manifest()
	if err != nil {
		return errors.Wrapf(err, "Error reading manifest for %s", transports.ImageName(srcRef))
	}
	multiImage, err := isMultiImage(unparsedToplevel)
	if err != nil {
		return errors.Wrapf(err, "Error determining manifest MIME type for %s", transports.ImageName(srcRef))
	}

	if !multiImage {
		// The simple case: just copy a single image.
		if _, _, err := c.copyOneImage(policyContext, options, unparsedToplevel, nil, nil); err != nil {
			return err
		}
	} else {
		// If we were asked to copy multiple images and can't, that's an error.
		if options.MultipleImages == CopyOnlyCurrentRuntimeImage {
			// This is a manifest list, and we weren't asked to copy multiple images.  Choose a single image to copy,
			// and copy it.
			manifestList, err := manifest.ListFromBlob(mfest, manifestType)
			if err != nil {
				return errors.Wrapf(err, "Error parsing primary manifest as list for %s", transports.ImageName(srcRef))
			}
			instanceDigest, err := manifestList.ChooseInstance(options.SourceCtx)
			if err != nil {
				return errors.Wrapf(err, "Error choosing an image from manifest list %s", transports.ImageName(srcRef))
			}
			logrus.Debugf("Source is a manifest list; copying (only) instance %s", instanceDigest)
			unparsedInstance := image.UnparsedInstance(rawSource, &instanceDigest)

			if _, _, err := c.copyOneImage(policyContext, options, unparsedInstance, &instanceDigest, nil); err != nil {
				return err
			}
		} else { /* options.MultipleImages == CopyAllImages */
			if !supportsMultipleImages(c.dest) {
				return errors.Errorf("Error copying multiple images: destination reference %q does not support multiple images", transports.ImageName(destRef))
			}
			// Copy all of the images.
			logrus.Debugf("Source is a manifest list; copying all instances")
			if err := c.copyMultipleImages(policyContext, options, unparsedToplevel); err != nil {
				return err
			}
		}
	}

	if err := c.dest.Commit(); err != nil {
		return errors.Wrap(err, "Error committing the finished image")
	}

	return nil
}

// Checks if the destination supports accepting multiple images by checking if it can support
// manifest types that are lists of other manifests.
func supportsMultipleImages(dest types.ImageDestination) bool {
	mtypes := dest.SupportedManifestMIMETypes()
	if len(mtypes) == 0 {
		// Anything goes!
		return true
	}
	for _, mtype := range mtypes {
		if manifest.MIMETypeIsMultiImage(mtype) {
			return true
		}
	}
	return false
}

// copyMultipleImages copies all of an image's instances, using policyContext to validate
// source image admissibility.
func (c *copier) copyMultipleImages(policyContext *signature.PolicyContext, options *Options, unparsedToplevel *image.UnparsedImage) (retErr error) {
	// Parse the list and get a copy of the original value after it's re-encoded.
	manifestList, manifestType, err := unparsedToplevel.Manifest()
	if err != nil {
		return errors.Wrapf(err, "Error reading manifest list")
	}
	list, err := manifest.ListFromBlob(manifestList, manifestType)
	if err != nil {
		return errors.Wrapf(err, "Error parsing manifest list %q", string(manifestList))
	}
	originalList := list.Clone()

	// Read and/or clear the set of signatures for this list.
	var sigs [][]byte
	if options.RemoveSignatures {
		sigs = [][]byte{}
	} else {
		c.Printf("Getting image list signatures\n")
		s, err := c.rawSource.GetSignatures(context.TODO(), nil)
		if err != nil {
			return errors.Wrap(err, "Error reading signatures")
		}
		sigs = s
	}
	if len(sigs) != 0 {
		c.Printf("Checking if image list destination supports signatures\n")
		if err := c.dest.SupportsSignatures(); err != nil {
			return errors.Wrap(err, "Can not copy signatures")
		}
	}

	// Copy each image, in turn.
	instanceBlobs := list.Instances()
	updates := make([]manifest.ListUpdate, len(instanceBlobs))
	for i, instance := range instanceBlobs {
		logrus.Debugf("Copying instance %s (%d/%d)", instance.Digest, i+1, len(instanceBlobs))
		unparsedInstance := image.UnparsedInstance(c.rawSource, &instance.Digest)
		updatedManifest, updatedManifestType, err := c.copyOneImage(policyContext, options, unparsedInstance, &instance.Digest, &instance.Digest)
		if err != nil {
			return err
		}
		// Record the result of a possible conversion here.
		md, err := manifest.Digest(updatedManifest)
		if err != nil {
			return err
		}
		update := manifest.ListUpdate{
			Digest:    md,
			Size:      int64(len(updatedManifest)),
			MediaType: updatedManifestType,
		}
		updates[i] = update
	}

	// Now apply the updates.
	if err = list.UpdateInstances(updates); err != nil {
		return errors.Wrapf(err, "Error updating manifest list")
	}

	// Check if the updates meaningfully changed the list of images.
	listIsModified := false
	if !reflect.DeepEqual(list.Instances(), originalList.Instances()) {
		listIsModified = true
	}

	// Determine if we need to convert the manifest to a different format.
	selectedType, _, err := c.determineListConversion(manifestType, c.dest.SupportedManifestMIMETypes(), "")
	if err != nil {
		return errors.Wrapf(err, "Error determining manifest list type to write to destination")
	}
	if selectedType != list.MIMEType() {
		listIsModified = true
		switch selectedType {
		case manifest.DockerV2ListMediaType:
			list, err = list.ToSchema2List()
			if err != nil {
				return errors.Wrapf(err, "Error converting manifest list to a schema 2 list")
			}
		case imgspecv1.MediaTypeImageIndex:
			list, err = list.ToOCI1Index()
			if err != nil {
				return errors.Wrapf(err, "Error converting manifest list to an oci 1 index")
			}
		default:
			return errors.Errorf("Error converting manifest list to unknown supported type %q", selectedType)
		}
	}

	// If we can't use the original value, but we have to change it, flag an error.
	if listIsModified {
		canModifyManifest := (len(sigs) == 0)
		if !canModifyManifest {
			return errors.Errorf("Internal error: copyMultipleImages() needs to use an updated manifest but that was known to be forbidden")
		}
		manifestList, err = list.Serialize()
		if err != nil {
			return errors.Wrapf(err, "Error encoding updated manifest list (%q: %#v)", list.MIMEType(), list.Instances())
		}
		logrus.Debugf("Manifest list has been updated")
	}

	// Save the manifest list.
	if err = c.dest.PutManifest(manifestList, nil); err != nil {
		return errors.Wrapf(err, "Error writing manifest list %q", string(manifestList))
	}

	// Sign the manifest list.
	if options.SignBy != "" {
		newSig, err := c.createSignature(manifestList, options.SignBy)
		if err != nil {
			return err
		}
		sigs = append(sigs, newSig)
	}

	c.Printf("Storing list signatures\n")
	if err := c.dest.PutSignatures(sigs, nil); err != nil {
		return errors.Wrap(err, "Error writing signatures")
	}

	return nil
}

// copyOneImage copies a single (non-manifest-list) image unparsedImage, using policyContext to validate
// source image admissibility.
func (c *copier) copyOneImage(policyContext *signature.PolicyContext, options *Options, unparsedImage *image.UnparsedImage, sourceInstance, targetInstance *digest.Digest) (retManifest []byte, retManifestType string, retErr error) {
	// The caller is handling manifest lists; this could happen only if a manifest list contains a manifest list.
	// Make sure we fail cleanly in such cases.
	multiImage, err := isMultiImage(unparsedImage)
	if err != nil {
		// FIXME FIXME: How to name a reference for the sub-image?
		return nil, "", errors.Wrapf(err, "Error determining manifest MIME type for %s", transports.ImageName(unparsedImage.Reference()))
	}
	if multiImage {
		return nil, "", fmt.Errorf("Unexpectedly received a manifest list instead of a manifest for a single image")
	}

	// Please keep this policy check BEFORE reading any other information about the image.
	// (The multiImage check above only matches the MIME type, which we have received anyway.
	// Actual parsing of anything should be deferred.)
	if allowed, err := policyContext.IsRunningImageAllowed(unparsedImage); !allowed || err != nil { // Be paranoid and fail if either return value indicates so.
		return nil, "", errors.Wrap(err, "Source image rejected")
	}
	src, err := image.FromUnparsedImage(options.SourceCtx, unparsedImage)
	if err != nil {
		return nil, "", errors.Wrapf(err, "Error initializing image from source %s", transports.ImageName(c.rawSource.Reference()))
	}

	if err := checkImageDestinationForCurrentRuntimeOS(options.DestinationCtx, src, c.dest); err != nil {
		return nil, "", err
	}

	var sigs [][]byte
	if options.RemoveSignatures {
		sigs = [][]byte{}
	} else {
		c.Printf("Getting image source signatures\n")
		s, err := src.Signatures(context.TODO())
		if err != nil {
			return nil, "", errors.Wrap(err, "Error reading signatures")
		}
		sigs = s
	}
	if len(sigs) != 0 {
		c.Printf("Checking if image destination supports signatures\n")
		if err := c.dest.SupportsSignatures(); err != nil {
			return nil, "", errors.Wrap(err, "Can not copy signatures")
		}
	}

	ic := imageCopier{
		c:               c,
		manifestUpdates: &types.ManifestUpdateOptions{InformationOnly: types.ManifestUpdateInformation{Destination: c.dest}},
		src:             src,
		// diffIDsAreNeeded is computed later
		canModifyManifest: len(sigs) == 0,
	}

	if err := ic.updateEmbeddedDockerReference(); err != nil {
		return nil, "", err
	}

	// We compute preferredManifestMIMEType only to show it in error messages.
	// Without having to add this context in an error message, we would be happy enough to know only that no conversion is needed.
	preferredManifestMIMEType, otherManifestMIMETypeCandidates, err := ic.determineManifestConversion(c.dest.SupportedManifestMIMETypes(), options.ForceManifestMIMEType)
	if err != nil {
		return nil, "", err
	}

	// If src.UpdatedImageNeedsLayerDiffIDs(ic.manifestUpdates) will be true, it needs to be true by the time we get here.
	ic.diffIDsAreNeeded = src.UpdatedImageNeedsLayerDiffIDs(*ic.manifestUpdates)

	if err := ic.copyLayers(); err != nil {
		return nil, "", err
	}

	// With docker/distribution registries we do not know whether the registry accepts schema2 or schema1 only;
	// and at least with the OpenShift registry "acceptschema2" option, there is no way to detect the support
	// without actually trying to upload something and getting a types.ManifestTypeRejectedError.
	// So, try the preferred manifest MIME type. If the process succeeds, fine…
	manifest, err := ic.copyUpdatedConfigAndManifest(targetInstance)
	retManifestType = preferredManifestMIMEType
	if err != nil {
		logrus.Debugf("Writing manifest using preferred type %s failed: %v", preferredManifestMIMEType, err)
		// … if it fails, _and_ the failure is because the manifest is rejected, we may have other options.
		if _, isManifestRejected := errors.Cause(err).(types.ManifestTypeRejectedError); !isManifestRejected || len(otherManifestMIMETypeCandidates) == 0 {
			// We don’t have other options.
			// In principle the code below would handle this as well, but the resulting  error message is fairly ugly.
			// Don’t bother the user with MIME types if we have no choice.
			return nil, "", err
		}
		// If the original MIME type is acceptable, determineManifestConversion always uses it as preferredManifestMIMEType.
		// So if we are here, we will definitely be trying to convert the manifest.
		// With !ic.canModifyManifest, that would just be a string of repeated failures for the same reason,
		// so let’s bail out early and with a better error message.
		if !ic.canModifyManifest {
			return nil, "", errors.Wrap(err, "Writing manifest failed (and converting it is not possible)")
		}

		// errs is a list of errors when trying various manifest types. Also serves as an "upload succeeded" flag when set to nil.
		errs := []string{fmt.Sprintf("%s(%v)", preferredManifestMIMEType, err)}
		for _, manifestMIMEType := range otherManifestMIMETypeCandidates {
			logrus.Debugf("Trying to use manifest type %s…", manifestMIMEType)
			ic.manifestUpdates.ManifestMIMEType = manifestMIMEType
			attemptedManifest, err := ic.copyUpdatedConfigAndManifest(targetInstance)
			if err != nil {
				logrus.Debugf("Upload of manifest type %s failed: %v", manifestMIMEType, err)
				errs = append(errs, fmt.Sprintf("%s(%v)", manifestMIMEType, err))
				continue
			}

			// We have successfully uploaded a manifest.
			manifest = attemptedManifest
			retManifestType = manifestMIMEType
			errs = nil // Mark this as a success so that we don't abort below.
			break
		}
		if errs != nil {
			return nil, "", fmt.Errorf("Uploading manifest failed, attempted the following formats: %s", strings.Join(errs, ", "))
		}
	}

	if options.SignBy != "" {
		newSig, err := c.createSignature(manifest, options.SignBy)
		if err != nil {
			return nil, "", err
		}
		sigs = append(sigs, newSig)
	}

	c.Printf("Storing signatures\n")
	if err := c.dest.PutSignatures(sigs, targetInstance); err != nil {
		return nil, "", errors.Wrap(err, "Error writing signatures")
	}

	return manifest, retManifestType, nil
}

// Printf writes a formatted string to c.reportWriter.
// Note that the method name Printf is not entirely arbitrary: (go tool vet)
// has a built-in list of functions/methods (whatever object they are for)
// which have their format strings checked; for other names we would have
// to pass a parameter to every (go tool vet) invocation.
func (c *copier) Printf(format string, a ...interface{}) {
	fmt.Fprintf(c.reportWriter, format, a...)
}

func checkImageDestinationForCurrentRuntimeOS(ctx *types.SystemContext, src types.Image, dest types.ImageDestination) error {
	if dest.MustMatchRuntimeOS() {
		wantedOS := runtime.GOOS
		if ctx != nil && ctx.OSChoice != "" {
			wantedOS = ctx.OSChoice
		}
		c, err := src.OCIConfig()
		if err != nil {
			return errors.Wrapf(err, "Error parsing image configuration")
		}
		osErr := fmt.Errorf("image operating system %q cannot be used on %q", c.OS, wantedOS)
		if wantedOS == "windows" && c.OS == "linux" {
			return osErr
		} else if wantedOS != "windows" && c.OS == "windows" {
			return osErr
		}
	}
	return nil
}

// updateEmbeddedDockerReference handles the Docker reference embedded in Docker schema1 manifests.
func (ic *imageCopier) updateEmbeddedDockerReference() error {
	destRef := ic.c.dest.Reference().DockerReference()
	if destRef == nil {
		return nil // Destination does not care about Docker references
	}
	if !ic.src.EmbeddedDockerReferenceConflicts(destRef) {
		return nil // No reference embedded in the manifest, or it matches destRef already.
	}

	if !ic.canModifyManifest {
		return errors.Errorf("Copying a schema1 image with an embedded Docker reference to %s (Docker reference %s) would invalidate existing signatures. Explicitly enable signature removal to proceed anyway",
			transports.ImageName(ic.c.dest.Reference()), destRef.String())
	}
	ic.manifestUpdates.EmbeddedDockerReference = destRef
	return nil
}

// copyLayers copies layers from ic.src/ic.c.rawSource to dest, using and updating ic.manifestUpdates if necessary and ic.canModifyManifest.
func (ic *imageCopier) copyLayers() error {
	srcInfos := ic.src.LayerInfos()
	destInfos := []types.BlobInfo{}
	diffIDs := []digest.Digest{}
	updatedSrcInfos := ic.src.LayerInfosForCopy()
	srcInfosUpdated := false
	if updatedSrcInfos != nil && !reflect.DeepEqual(srcInfos, updatedSrcInfos) {
		if !ic.canModifyManifest {
			return errors.Errorf("Internal error: copyLayers() needs to use an updated manifest but that was known to be forbidden")
		}
		srcInfos = updatedSrcInfos
		srcInfosUpdated = true
	}
	for _, srcLayer := range srcInfos {
		var (
			destInfo types.BlobInfo
			diffID   digest.Digest
			err      error
		)
		if ic.c.dest.AcceptsForeignLayerURLs() && len(srcLayer.URLs) != 0 {
			// DiffIDs are, currently, needed only when converting from schema1.
			// In which case src.LayerInfos will not have URLs because schema1
			// does not support them.
			if ic.diffIDsAreNeeded {
				return errors.New("getting DiffID for foreign layers is unimplemented")
			}
			destInfo = srcLayer
			ic.c.Printf("Skipping foreign layer %q copy to %s\n", destInfo.Digest, ic.c.dest.Reference().Transport().Name())
		} else {
			destInfo, diffID, err = ic.copyLayer(srcLayer)
			if err != nil {
				return err
			}
		}
		destInfos = append(destInfos, destInfo)
		diffIDs = append(diffIDs, diffID)
	}
	ic.manifestUpdates.InformationOnly.LayerInfos = destInfos
	if ic.diffIDsAreNeeded {
		ic.manifestUpdates.InformationOnly.LayerDiffIDs = diffIDs
	}
	if srcInfosUpdated || layerDigestsDiffer(srcInfos, destInfos) {
		ic.manifestUpdates.LayerInfos = destInfos
	}
	return nil
}

// layerDigestsDiffer return true iff the digests in a and b differ (ignoring sizes and possible other fields)
func layerDigestsDiffer(a, b []types.BlobInfo) bool {
	if len(a) != len(b) {
		return true
	}
	for i := range a {
		if a[i].Digest != b[i].Digest {
			return true
		}
	}
	return false
}

// copyUpdatedConfigAndManifest updates the image per ic.manifestUpdates, if necessary,
// stores the resulting config and manifest to the destination, and returns the stored manifest.
func (ic *imageCopier) copyUpdatedConfigAndManifest(instanceDigest *digest.Digest) ([]byte, error) {
	pendingImage := ic.src
	if !reflect.DeepEqual(*ic.manifestUpdates, types.ManifestUpdateOptions{InformationOnly: ic.manifestUpdates.InformationOnly}) {
		if !ic.canModifyManifest {
			return nil, errors.Errorf("Internal error: copy needs an updated manifest but that was known to be forbidden")
		}
		if !ic.diffIDsAreNeeded && ic.src.UpdatedImageNeedsLayerDiffIDs(*ic.manifestUpdates) {
			// We have set ic.diffIDsAreNeeded based on the preferred MIME type returned by determineManifestConversion.
			// So, this can only happen if we are trying to upload using one of the other MIME type candidates.
			// Because UpdatedImageNeedsLayerDiffIDs is true only when converting from s1 to s2, this case should only arise
			// when ic.c.dest.SupportedManifestMIMETypes() includes both s1 and s2, the upload using s1 failed, and we are now trying s2.
			// Supposedly s2-only registries do not exist or are extremely rare, so failing with this error message is good enough for now.
			// If handling such registries turns out to be necessary, we could compute ic.diffIDsAreNeeded based on the full list of manifest MIME type candidates.
			return nil, errors.Errorf("Can not convert image to %s, preparing DiffIDs for this case is not supported", ic.manifestUpdates.ManifestMIMEType)
		}
		pi, err := ic.src.UpdatedImage(*ic.manifestUpdates)
		if err != nil {
			return nil, errors.Wrap(err, "Error creating an updated image manifest")
		}
		pendingImage = pi
	}
	man, _, err := pendingImage.Manifest()
	if err != nil {
		return nil, errors.Wrap(err, "Error reading manifest")
	}

	if err := ic.c.copyConfig(pendingImage); err != nil {
		return nil, err
	}

	ic.c.Printf("Writing manifest to image destination\n")
	if instanceDigest != nil {
		tmpDigest, err := manifest.Digest(man)
		if err != nil {
			return nil, err
		}
		instanceDigest = &tmpDigest
	}
	if err := ic.c.dest.PutManifest(man, instanceDigest); err != nil {
		return nil, errors.Wrap(err, "Error writing manifest")
	}
	return man, nil
}

// copyConfig copies config.json, if any, from src to dest.
func (c *copier) copyConfig(src types.Image) error {
	srcInfo := src.ConfigInfo()
	if srcInfo.Digest != "" {
		c.Printf("Copying config %s\n", srcInfo.Digest)
		configBlob, err := src.ConfigBlob()
		if err != nil {
			return errors.Wrapf(err, "Error reading config blob %s", srcInfo.Digest)
		}
		destInfo, err := c.copyBlobFromStream(bytes.NewReader(configBlob), srcInfo, nil, false)
		if err != nil {
			return err
		}
		if destInfo.Digest != srcInfo.Digest {
			return errors.Errorf("Internal error: copying uncompressed config blob %s changed digest to %s", srcInfo.Digest, destInfo.Digest)
		}
	}
	return nil
}

// diffIDResult contains both a digest value and an error from diffIDComputationGoroutine.
// We could also send the error through the pipeReader, but this more cleanly separates the copying of the layer and the DiffID computation.
type diffIDResult struct {
	digest digest.Digest
	err    error
}

// copyLayer copies a layer with srcInfo (with known Digest and possibly known Size) in src to dest, perhaps compressing it if canCompress,
// and returns a complete blobInfo of the copied layer, and a value for LayerDiffIDs if diffIDIsNeeded
func (ic *imageCopier) copyLayer(srcInfo types.BlobInfo) (types.BlobInfo, digest.Digest, error) {
	// Check if we already have a blob with this digest
	haveBlob, extantBlobSize, err := ic.c.dest.HasBlob(srcInfo)
	if err != nil {
		return types.BlobInfo{}, "", errors.Wrapf(err, "Error checking for blob %s at destination", srcInfo.Digest)
	}
	// If we already have a cached diffID for this blob, we don't need to compute it
	diffIDIsNeeded := ic.diffIDsAreNeeded && (ic.c.cachedDiffIDs[srcInfo.Digest] == "")
	// If we already have the blob, and we don't need to recompute the diffID, then we might be able to avoid reading it again
	if haveBlob && !diffIDIsNeeded {
		// Check the blob sizes match, if we were given a size this time
		if srcInfo.Size != -1 && srcInfo.Size != extantBlobSize {
			return types.BlobInfo{}, "", errors.Errorf("Error: blob %s is already present, but with size %d instead of %d", srcInfo.Digest, extantBlobSize, srcInfo.Size)
		}
		srcInfo.Size = extantBlobSize
		// Tell the image destination that this blob's delta is being applied again.  For some image destinations, this can be faster than using GetBlob/PutBlob
		blobinfo, err := ic.c.dest.ReapplyBlob(srcInfo)
		if err != nil {
			return types.BlobInfo{}, "", errors.Wrapf(err, "Error reapplying blob %s at destination", srcInfo.Digest)
		}
		ic.c.Printf("Skipping fetch of repeat blob %s\n", srcInfo.Digest)
		return blobinfo, ic.c.cachedDiffIDs[srcInfo.Digest], err
	}

	// Fallback: copy the layer, computing the diffID if we need to do so
	ic.c.Printf("Copying blob %s\n", srcInfo.Digest)
	srcStream, srcBlobSize, err := ic.c.rawSource.GetBlob(srcInfo)
	if err != nil {
		return types.BlobInfo{}, "", errors.Wrapf(err, "Error reading blob %s", srcInfo.Digest)
	}
	defer srcStream.Close()

	blobInfo, diffIDChan, err := ic.copyLayerFromStream(srcStream, types.BlobInfo{Digest: srcInfo.Digest, Size: srcBlobSize},
		diffIDIsNeeded)
	if err != nil {
		return types.BlobInfo{}, "", err
	}
	var diffIDResult diffIDResult // = {digest:""}
	if diffIDIsNeeded {
		diffIDResult = <-diffIDChan
		if diffIDResult.err != nil {
			return types.BlobInfo{}, "", errors.Wrap(diffIDResult.err, "Error computing layer DiffID")
		}
		logrus.Debugf("Computed DiffID %s for layer %s", diffIDResult.digest, srcInfo.Digest)
		ic.c.cachedDiffIDs[srcInfo.Digest] = diffIDResult.digest
	}
	return blobInfo, diffIDResult.digest, nil
}

// copyLayerFromStream is an implementation detail of copyLayer; mostly providing a separate “defer” scope.
// it copies a blob with srcInfo (with known Digest and possibly known Size) from srcStream to dest,
// perhaps compressing the stream if canCompress,
// and returns a complete blobInfo of the copied blob and perhaps a <-chan diffIDResult if diffIDIsNeeded, to be read by the caller.
func (ic *imageCopier) copyLayerFromStream(srcStream io.Reader, srcInfo types.BlobInfo,
	diffIDIsNeeded bool) (types.BlobInfo, <-chan diffIDResult, error) {
	var getDiffIDRecorder func(compression.DecompressorFunc) io.Writer // = nil
	var diffIDChan chan diffIDResult

	err := errors.New("Internal error: unexpected panic in copyLayer") // For pipeWriter.CloseWithError below
	if diffIDIsNeeded {
		diffIDChan = make(chan diffIDResult, 1) // Buffered, so that sending a value after this or our caller has failed and exited does not block.
		pipeReader, pipeWriter := io.Pipe()
		defer func() { // Note that this is not the same as {defer pipeWriter.CloseWithError(err)}; we need err to be evaluated lazily.
			pipeWriter.CloseWithError(err) // CloseWithError(nil) is equivalent to Close()
		}()

		getDiffIDRecorder = func(decompressor compression.DecompressorFunc) io.Writer {
			// If this fails, e.g. because we have exited and due to pipeWriter.CloseWithError() above further
			// reading from the pipe has failed, we don’t really care.
			// We only read from diffIDChan if the rest of the flow has succeeded, and when we do read from it,
			// the return value includes an error indication, which we do check.
			//
			// If this gets never called, pipeReader will not be used anywhere, but pipeWriter will only be
			// closed above, so we are happy enough with both pipeReader and pipeWriter to just get collected by GC.
			go diffIDComputationGoroutine(diffIDChan, pipeReader, decompressor) // Closes pipeReader
			return pipeWriter
		}
	}
	blobInfo, err := ic.c.copyBlobFromStream(srcStream, srcInfo, getDiffIDRecorder, ic.canModifyManifest) // Sets err to nil on success
	return blobInfo, diffIDChan, err
	// We need the defer … pipeWriter.CloseWithError() to happen HERE so that the caller can block on reading from diffIDChan
}

// diffIDComputationGoroutine reads all input from layerStream, uncompresses using decompressor if necessary, and sends its digest, and status, if any, to dest.
func diffIDComputationGoroutine(dest chan<- diffIDResult, layerStream io.ReadCloser, decompressor compression.DecompressorFunc) {
	result := diffIDResult{
		digest: "",
		err:    errors.New("Internal error: unexpected panic in diffIDComputationGoroutine"),
	}
	defer func() { dest <- result }()
	defer layerStream.Close() // We do not care to bother the other end of the pipe with other failures; we send them to dest instead.

	result.digest, result.err = computeDiffID(layerStream, decompressor)
}

// computeDiffID reads all input from layerStream, uncompresses it using decompressor if necessary, and returns its digest.
func computeDiffID(stream io.Reader, decompressor compression.DecompressorFunc) (digest.Digest, error) {
	if decompressor != nil {
		s, err := decompressor(stream)
		if err != nil {
			return "", err
		}
		stream = s
	}

	return digest.Canonical.FromReader(stream)
}

// copyBlobFromStream copies a blob with srcInfo (with known Digest and possibly known Size) from srcStream to dest,
// perhaps sending a copy to an io.Writer if getOriginalLayerCopyWriter != nil,
// perhaps compressing it if canCompress,
// and returns a complete blobInfo of the copied blob.
func (c *copier) copyBlobFromStream(srcStream io.Reader, srcInfo types.BlobInfo,
	getOriginalLayerCopyWriter func(decompressor compression.DecompressorFunc) io.Writer,
	canCompress bool) (types.BlobInfo, error) {
	// The copying happens through a pipeline of connected io.Readers.
	// === Input: srcStream

	// === Process input through digestingReader to validate against the expected digest.
	// Be paranoid; in case PutBlob somehow managed to ignore an error from digestingReader,
	// use a separate validation failure indicator.
	// Note that we don't use a stronger "validationSucceeded" indicator, because
	// dest.PutBlob may detect that the layer already exists, in which case we don't
	// read stream to the end, and validation does not happen.
	digestingReader, err := newDigestingReader(srcStream, srcInfo.Digest)
	if err != nil {
		return types.BlobInfo{}, errors.Wrapf(err, "Error preparing to verify blob %s", srcInfo.Digest)
	}
	var destStream io.Reader = digestingReader

	// === Detect compression of the input stream.
	// This requires us to “peek ahead” into the stream to read the initial part, which requires us to chain through another io.Reader returned by DetectCompression.
	decompressor, destStream, err := compression.DetectCompression(destStream) // We could skip this in some cases, but let's keep the code path uniform
	if err != nil {
		return types.BlobInfo{}, errors.Wrapf(err, "Error reading blob %s", srcInfo.Digest)
	}
	isCompressed := decompressor != nil

	// === Report progress using a pb.Reader.
	bar := pb.New(int(srcInfo.Size)).SetUnits(pb.U_BYTES)
	bar.Output = c.reportWriter
	bar.SetMaxWidth(80)
	bar.ShowTimeLeft = false
	bar.ShowPercent = false
	bar.Start()
	destStream = bar.NewProxyReader(destStream)
	defer bar.Finish()

	// === Send a copy of the original, uncompressed, stream, to a separate path if necessary.
	var originalLayerReader io.Reader // DO NOT USE this other than to drain the input if no other consumer in the pipeline has done so.
	if getOriginalLayerCopyWriter != nil {
		destStream = io.TeeReader(destStream, getOriginalLayerCopyWriter(decompressor))
		originalLayerReader = destStream
	}

	// === Compress the layer if it is uncompressed and compression is desired
	var inputInfo types.BlobInfo
	if !canCompress || isCompressed || !c.dest.ShouldCompressLayers() {
		logrus.Debugf("Using original blob without modification")
		inputInfo = srcInfo
	} else {
		logrus.Debugf("Compressing blob on the fly")
		pipeReader, pipeWriter := io.Pipe()
		defer pipeReader.Close()

		// If this fails while writing data, it will do pipeWriter.CloseWithError(); if it fails otherwise,
		// e.g. because we have exited and due to pipeReader.Close() above further writing to the pipe has failed,
		// we don’t care.
		go compressGoroutine(pipeWriter, destStream) // Closes pipeWriter
		destStream = pipeReader
		inputInfo.Digest = ""
		inputInfo.Size = -1
	}

	// === Report progress using the c.progress channel, if required.
	if c.progress != nil && c.progressInterval > 0 {
		destStream = &progressReader{
			source:   destStream,
			channel:  c.progress,
			interval: c.progressInterval,
			artifact: srcInfo,
			lastTime: time.Now(),
		}
	}

	// === Finally, send the layer stream to dest.
	uploadedInfo, err := c.dest.PutBlob(destStream, inputInfo)
	if err != nil {
		return types.BlobInfo{}, errors.Wrap(err, "Error writing blob")
	}

	// This is fairly horrible: the writer from getOriginalLayerCopyWriter wants to consumer
	// all of the input (to compute DiffIDs), even if dest.PutBlob does not need it.
	// So, read everything from originalLayerReader, which will cause the rest to be
	// sent there if we are not already at EOF.
	if getOriginalLayerCopyWriter != nil {
		logrus.Debugf("Consuming rest of the original blob to satisfy getOriginalLayerCopyWriter")
		_, err := io.Copy(ioutil.Discard, originalLayerReader)
		if err != nil {
			return types.BlobInfo{}, errors.Wrapf(err, "Error reading input blob %s", srcInfo.Digest)
		}
	}

	if digestingReader.validationFailed { // Coverage: This should never happen.
		return types.BlobInfo{}, errors.Errorf("Internal error writing blob %s, digest verification failed but was ignored", srcInfo.Digest)
	}
	if inputInfo.Digest != "" && uploadedInfo.Digest != inputInfo.Digest {
		return types.BlobInfo{}, errors.Errorf("Internal error writing blob %s, blob with digest %s saved with digest %s", srcInfo.Digest, inputInfo.Digest, uploadedInfo.Digest)
	}
	return uploadedInfo, nil
}

// compressGoroutine reads all input from src and writes its compressed equivalent to dest.
func compressGoroutine(dest *io.PipeWriter, src io.Reader) {
	err := errors.New("Internal error: unexpected panic in compressGoroutine")
	defer func() { // Note that this is not the same as {defer dest.CloseWithError(err)}; we need err to be evaluated lazily.
		dest.CloseWithError(err) // CloseWithError(nil) is equivalent to Close()
	}()

	zipper := gzip.NewWriter(dest)
	defer zipper.Close()

	_, err = io.Copy(zipper, src) // Sets err to nil, i.e. causes dest.Close()
}
