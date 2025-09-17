package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"k8s.io/kube-openapi/pkg/common"
	"k8s.io/kube-openapi/pkg/spec3"
	"k8s.io/kube-openapi/pkg/validation/spec"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	"github.com/dominodatalab/hephaestus/pkg/crd"
	"github.com/dominodatalab/hephaestus/pkg/kubernetes"
)

const swaggerPath = "github.com/dominodatalab/hephaestus/api/openapi-spec/swagger3.json"

var (
	libRE = regexp.MustCompile(`github\.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1\.`)
	k8sRE = regexp.MustCompile(`k8s\.io/apimachinery/pkg/apis/meta/`)
)

func main() {
	createKindCluster()
	defer deleteKindCluster()

	doc := getOpenAPIV3Document()

	bs, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		log.Println(err)
		return
	}
	if err = os.WriteFile(swaggerPath, bs, 0644); err != nil {
		log.Println(err)
		return
	}

	generate()
}

func createKindCluster() {
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "kind", "create", "cluster", "--config", "scripts/sdk/kind.yaml")
	if err := cmd.Run(); err != nil {
		log.Fatalln(err)
	}

	if err := crd.Apply(context.Background()); err != nil {
		log.Fatalln(err)
	}
}

func deleteKindCluster() {
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "kind", "delete", "cluster")
	if err := cmd.Run(); err != nil {
		log.Fatalln(err)
	}
}

func getOpenAPIV3Document() *spec3.OpenAPI {
	/*
		query k8s for hephaestus openapiv3 spec
	*/
	clientset, err := kubernetes.Clientset(nil)
	if err != nil {
		log.Fatalln(err)
	}

	data, err := clientset.
		Discovery().
		RESTClient().
		Get().
		RequestURI("/openapi/v3/apis/hephaestus.dominodatalab.com/v1").
		Do(context.Background()).
		Raw()
	if err != nil {
		log.Fatalln(err)
	}

	var openapiv3 spec3.OpenAPI
	if err = json.Unmarshal(data, &openapiv3); err != nil {
		log.Fatalln(err)
	}

	/*
		modify routes
	*/
	for name, path := range openapiv3.Paths.Paths {
		var tags []string
		switch {
		case strings.Contains(name, "imagebuilds"):
			tags = []string{"ImageBuildService"}
		case strings.Contains(name, "imagecaches"):
			tags = []string{"ImageCacheService"}
		}

		if path.Get != nil {
			mutateOperation(path.Get)
			path.Get.Tags = tags
		}
	}

	for name, path := range openapiv3.Paths.Paths {
		var tags []string
		switch {
		case strings.Contains(name, "imagebuilds"):
			tags = []string{"ImageBuildService"}
		case strings.Contains(name, "imagecaches"):
			tags = []string{"ImageCacheService"}
		}

		if path.Get != nil {
			path.Get.Tags = tags
		}
		if path.Post != nil {
			path.Post.Tags = tags
		}
		if path.Put != nil {
			path.Put.Tags = tags
		}
		if path.Delete != nil {
			path.Delete.Tags = tags
		}
		if path.Patch != nil {
			path.Patch.Tags = tags
		}
	}

	defs := hephv1.GetOpenAPIDefinitions(func(path string) spec.Ref {
		switch {
		case libRE.MatchString(path):
			path = libRE.ReplaceAllString(path, "")
		case k8sRE.MatchString(path):
			path = k8sRE.ReplaceAllString(path, "")
		}
		return spec.MustCreateRef("#/components/schemas/" + common.EscapeJsonPointer(path))
	})

	schemas := map[string]*spec.Schema{}
	for name, definition := range defs {
		def := definition
		schemas[libRE.ReplaceAllString(name, "")] = &def.Schema
	}
	openapiv3.Components.Schemas = schemas

	return &openapiv3
}

func mutateOperation(op *spec3.Operation) {
	if reqBody := op.RequestBody; reqBody != nil {
		for _, mediaType := range reqBody.Content {
			if schema := mediaType.Schema; schema != nil {
				schema.Ref = spec.MustCreateRef(pathRef(schema.Ref.String()))
			}
		}
	}

	for _, response := range op.Responses.StatusCodeResponses {
		for _, mediaType := range response.Content {
			if schema := mediaType.Schema; schema != nil {
				schema.Ref = spec.MustCreateRef(pathRef(schema.Ref.String()))
			}
		}
	}
}

func pathRef(path string) string {
	path = strings.Replace(path, "com.dominodatalab.hephaestus.v1.", "", 1)
	return path

	// name = strings.ReplaceAll(name, "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1", "")
	// name = strings.ReplaceAll(name, "k8s.io/apimachinery/pkg/apis/meta/", "")
	//
	// return name
}

// func walkOperation(op *spec3.Operation) {
// 	if reqBody := op.RequestBody; reqBody != nil {
// 		for _, mediaType := range reqBody.Content {
// 			if schema := mediaType.Schema; schema != nil {
// 				ref := schema.Ref
//
// 				switch {
// 				case strings.
// 				}
// 			}
// 		}
// 	}
//
// 	for _, response := range op.Responses.StatusCodeResponses {
// 		for _, mediaType := range response.Content {
// 			if schema := mediaType.Schema; schema != nil {
// 				schema.Ref = refFunc(schema.Ref.String())
// 			}
// 		}
// 	}
// }

func generate() {
	ctx := context.Background()
	cmd := exec.CommandContext(
		ctx,
		"openapi-generator",
		"generate",
		// "--skip-validate-spec",
		"--input-spec", swaggerPath,
		"--output", "better-gen-spec",
		"--config", "prototype/config.yaml",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		log.Fatalln(err)
	}
}
