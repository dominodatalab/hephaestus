package functional

import (
	"math/rand"
	"time"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

var seededRand = rand.New(rand.NewSource(time.Now().UnixNano()))

func RandomString(length int) string {
	bs := make([]byte, length)
	for i := range bs {
		bs[i] = charset[seededRand.Intn(len(charset))]
	}
	return string(bs)
}

func validImageBuild() *hephv1.ImageBuild {
	return &hephv1.ImageBuild{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-build-",
		},
		Spec: hephv1.ImageBuildSpec{
			Context: "https://raw.githubusercontent.com/dominodatalab/hephaestus/functional-testing/test/functional/fixtures/docker-context/python39-jupyter/archive.tgz",
			Images: []string{
				"registry/org/repo:tag",
			},
		},
	}
}
