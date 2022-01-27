package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"

	"k8s.io/kube-openapi/pkg/common"
	"k8s.io/kube-openapi/pkg/validation/spec"

	heph "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
)

func main() {
	var (
		jsonFile string
		version  string
	)

	flag.StringVar(&jsonFile, "json", "", "Kubernetes OpenAPI JSON")
	flag.StringVar(&version, "version", "", "API library version")
	flag.Parse()

	if jsonFile == "" || version == "" {
		flag.Usage()
		os.Exit(1)
	}

	swagger, err := processRawJSON(jsonFile)
	if err != nil {
		log.Fatalln(err)
	}

	modifyRoutes(swagger)
	modifyDefinitions(swagger)
	modifyProperties(swagger, version)

	bs, err := json.MarshalIndent(swagger, "", "  ")
	if err != nil {
		log.Fatalln(err)
	}

	fmt.Println(string(bs))
}

// processRawJSON reads a file containing the aggregated OpenAPI v2 JSON served by Kubernetes, transforms the raw bytes,
// and returns a swagger object for further processing.
func processRawJSON(path string) (*spec.Swagger, error) {
	bs, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	// remove "_vN" type suffixes created during CRD apply
	re := regexp.MustCompile(`(io\.k8s\.apimachinery\.pkg\.apis\.meta\.[^_]+)_v\d`)
	bs = re.ReplaceAll(bs, []byte("$1"))

	// compact library and metav1 references
	bs = bytes.ReplaceAll(bs, []byte("com.dominodatalab.hephaestus.v1"), []byte(""))
	bs = bytes.ReplaceAll(bs, []byte("io.k8s.apimachinery.pkg.apis.meta."), []byte(""))

	swagger := &spec.Swagger{}
	if err = json.Unmarshal(bs, swagger); err != nil {
		return nil, err
	}

	return swagger, nil
}

// modifyRoutes gathers project routes, modifies their operations, and sets them on the swagger object.
func modifyRoutes(swagger *spec.Swagger) {
	paths := map[string]spec.PathItem{}

	for name, item := range swagger.Paths.Paths {
		if !strings.HasPrefix(name, "/apis/hephaestus.dominodatalab.com") {
			continue
		}

		var tags []string
		switch {
		case strings.Contains(name, "imagebuilds"):
			tags = []string{"ImageBuildService"}
		case strings.Contains(name, "imagecaches"):
			tags = []string{"ImageCacheService"}
		}

		if item.Get != nil {
			modifyOperation(item.Get, tags)
		}
		if item.Post != nil {
			modifyOperation(item.Post, tags)
		}
		if item.Put != nil {
			modifyOperation(item.Put, tags)
		}
		if item.Delete != nil {
			modifyOperation(item.Delete, tags)
		}
		if item.Patch != nil {
			modifyOperation(item.Patch, tags)
		}

		paths[name] = item
	}
	swagger.Paths.Paths = paths
}

// modifyOperation affects generated function names and the generated services where they will reside.
func modifyOperation(op *spec.Operation, tags []string) {
	op.Tags = tags
	op.ID = strings.ReplaceAll(op.ID, "HephaestusDominodatalabComV1", "")
}

// modifyDefinitions renders OpenAPI definitions for project types and sets them on the swagger object.
func modifyDefinitions(swagger *spec.Swagger) {
	oAPIDefs := heph.GetOpenAPIDefinitions(func(path string) spec.Ref {
		return spec.MustCreateRef("#/definitions/" + common.EscapeJsonPointer(swaggerRef(path)))
	})

	defs := spec.Definitions{}
	for name, val := range oAPIDefs {
		defs[swaggerRef(name)] = val.Schema
	}

	swagger.Definitions = defs
}

// swaggerRef strips the canonical prefix from definition names.
//
// project types become ".<Type>" and meta types become "v1.<Type>"
func swaggerRef(name string) string {
	name = strings.ReplaceAll(name, "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1", "")
	name = strings.ReplaceAll(name, "k8s.io/apimachinery/pkg/apis/meta/", "")

	return name
}

// modifyProperties changes swagger properties other than paths and definitions.
func modifyProperties(swagger *spec.Swagger, version string) {
	swagger.Host = "localhost"
	swagger.Schemes = []string{
		"http",
		"https",
	}
	swagger.Info = &spec.Info{
		InfoProps: spec.InfoProps{
			Title:          "Hephaestus Kubernetes SDK",
			Description:    "Client APIs and models",
			TermsOfService: "https://www.dominodatalab.com/terms",
			Contact: &spec.ContactInfo{
				Name:  "Domino Data Lab, Inc.",
				URL:   "https://www.dominodatalab.com/",
				Email: "support@dominodatalab.com",
			},
			License: &spec.License{
				Name: "Apache 2.0",
				URL:  "https://www.apache.org/licenses/LICENSE-2.0",
			},
			Version: version,
		},
	}
	swagger.Security = nil
	swagger.SecurityDefinitions = nil
}
