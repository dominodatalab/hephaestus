package testenv

import (
	"context"
	"embed"
	"log"
	"os"
)

// a place to store runtime executables and files
const runtimePath = "/var/tmp/testenv"

//go:embed resources/*
var resources embed.FS

// logger for setup functions
var testenvLog = newLogger("testenv")

// Manager is responsible for governing the lifecycle of Kubernetes test environments including creation, destruction,
// application installation via Helmfile, and providing access to a kubeconfig file.
type Manager interface {
	// Create a new environment.
	Create(ctx context.Context) error
	// Destroy an existing environment.
	Destroy(ctx context.Context) error
	// HelmfileApply all resources from helmfile only when there are changes.
	HelmfileApply(ctx context.Context, helmfilePath string) error
	// KubeconfigBytes can be written to disk or used to initialize a Kubernetes client.
	KubeconfigBytes(ctx context.Context) ([]byte, error)
}

// creates a stdlib logger with an identifier
func newLogger(agent string) *log.Logger {
	return log.New(os.Stdout, "["+agent+"] ", log.Lmsgprefix|log.LstdFlags)
}
