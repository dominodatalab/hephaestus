package buildkit

import (
	"slices"

	"github.com/google/go-containerregistry/pkg/name"
)

// ParseImageReference parses imageName into a name.Reference, using the insecure (HTTP/self-signed
// TLS) parsing option when the image's registry host is in insecureRegistries.
func ParseImageReference(imageName string, insecureRegistries []string) (name.Reference, error) {
	ref, err := name.ParseReference(imageName)
	if err != nil {
		return nil, err
	}

	if slices.Contains(insecureRegistries, ref.Context().RegistryStr()) {
		return name.ParseReference(imageName, name.Insecure)
	}

	return ref, nil
}
