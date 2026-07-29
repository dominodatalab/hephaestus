package buildkit

import (
	"context"
	"errors"
	"fmt"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// PlatformImageRef associates a pushed single-platform image reference with the "os/arch[/variant]"
// platform it was built for.
type PlatformImageRef struct {
	Platform string
	ImageRef string
}

// AssembleManifestList resolves each per-platform image reference to its pushed image, combines
// them into a single OCI image index (manifest list), and pushes the index to finalRef. It returns
// the pushed index's digest.
//
// This is the manual equivalent of what a single multi-platform Solve produces automatically within
// one buildkit daemon (see BuildOptions.Platforms) - used when the requested platforms had to be
// built across more than one buildkit pool, so each platform's image was pushed independently and
// must be stitched into one manifest list here instead.
func (c *Client) AssembleManifestList(
	ctx context.Context,
	insecureRegistries []string,
	finalRef string,
	refs []PlatformImageRef,
) (string, error) {
	if len(refs) == 0 {
		return "", errors.New("no platform image references provided")
	}

	var idx v1.ImageIndex = empty.Index

	for _, r := range refs {
		img, err := c.retrieveImageForAssembly(ctx, r.ImageRef, insecureRegistries)
		if err != nil {
			return "", fmt.Errorf("cannot resolve platform image %q: %w", r.ImageRef, err)
		}

		platform, err := parsePlatform(r.Platform)
		if err != nil {
			return "", err
		}

		idx = mutate.AppendManifests(idx, mutate.IndexAddendum{
			Add:        img,
			Descriptor: v1.Descriptor{Platform: platform},
		})
	}

	ref, err := ParseImageReference(finalRef, insecureRegistries)
	if err != nil {
		return "", fmt.Errorf("cannot parse final reference %q: %w", finalRef, err)
	}

	auth, err := c.ResolveAuth(ctx, ref.Context().RegistryStr())
	if err != nil {
		return "", fmt.Errorf("cannot resolve auth for %q: %w", ref.Context().RegistryStr(), err)
	}

	if err := remote.WriteIndex(ref, idx, remote.WithContext(ctx), remote.WithAuth(auth)); err != nil {
		return "", fmt.Errorf("cannot push manifest list: %w", err)
	}

	digest, err := idx.Digest()
	if err != nil {
		return "", fmt.Errorf("cannot compute manifest list digest: %w", err)
	}

	return digest.String(), nil
}

// retrieveImageForAssembly resolves a pushed single-platform image reference to its v1.Image, using
// this client's registry auth (auth resolution is a local configDir/authProvider lookup, not a
// buildkitd RPC, so any client sharing the same configDir works regardless of which daemon
// produced the image).
func (c *Client) retrieveImageForAssembly(
	ctx context.Context, imageRef string, insecureRegistries []string,
) (v1.Image, error) {
	ref, err := ParseImageReference(imageRef, insecureRegistries)
	if err != nil {
		return nil, err
	}

	auth, err := c.ResolveAuth(ctx, ref.Context().RegistryStr())
	if err != nil {
		return nil, err
	}

	return remote.Image(ref, remote.WithContext(ctx), remote.WithAuth(auth))
}

// parsePlatform splits a normalized "os/arch[/variant]" platform string into a v1.Platform
// descriptor.
func parsePlatform(platform string) (*v1.Platform, error) {
	parts := strings.Split(platform, "/")
	if len(parts) < 2 || len(parts) > 3 {
		return nil, fmt.Errorf("platform %q must use \"os/arch\" or \"os/arch/variant\" syntax", platform)
	}

	p := &v1.Platform{OS: parts[0], Architecture: parts[1]}
	if len(parts) == 3 {
		p.Variant = parts[2]
	}

	return p, nil
}
