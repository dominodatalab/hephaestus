package crds

import "embed"

//go:embed *.yaml
var manifests embed.FS

// Definition represents the metadata and contents of a single custom resource definition.
type Definition struct {
	Filename string
	Contents []byte
}

// ReadAll returns a slice of custom resource Definition objects.
func ReadAll() ([]Definition, error) {
	files, err := manifests.ReadDir(".")
	if err != nil {
		return nil, err
	}

	var defs []Definition
	for _, f := range files {
		if f.IsDir() {
			continue
		}

		contents, err := manifests.ReadFile(f.Name())
		if err != nil {
			return nil, err
		}

		defs = append(defs, Definition{
			Filename: f.Name(),
			Contents: contents,
		})
	}

	return defs, nil
}
