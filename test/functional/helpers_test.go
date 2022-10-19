package functional

import (
	"fmt"
	"math/rand"
	"time"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var seededRand = rand.New(rand.NewSource(time.Now().UnixNano()))

const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func randomString(length int) string {
	bs := make([]byte, length)
	for i := range bs {
		bs[i] = charset[seededRand.Intn(len(charset))]
	}
	return string(bs)
}

type remoteDockerBuildContext int

const (
	contextServer = "https://raw.githubusercontent.com/dominodatalab/hephaestus/complete-gke-testing/test/functional/testdata/docker-context/%s/archive.tgz"

	buildArgBuildContext remoteDockerBuildContext = iota
	errorBuildContext
	python39JupyterBuildContext
)

func (c remoteDockerBuildContext) String() string {
	return [...]string{
		"build-arg",
		"error",
		"python39-jupyter",
	}[c-1]
}

func newImageBuild(bc remoteDockerBuildContext, image string, creds *hephv1.RegistryCredentials) *hephv1.ImageBuild {
	uid := randomString(8)
	dockerContextURL := fmt.Sprintf(contextServer, bc.String())

	var auth []hephv1.RegistryCredentials
	if creds != nil {
		auth = append(auth, *creds)
	}

	return &hephv1.ImageBuild{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-build-",
		},
		Spec: hephv1.ImageBuildSpec{
			Images:       []string{fmt.Sprintf("%s:%s", image, uid)},
			LogKey:       uid,
			Context:      dockerContextURL,
			RegistryAuth: auth,
		},
	}
}
