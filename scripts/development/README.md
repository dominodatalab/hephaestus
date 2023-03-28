
### Introduction

Hephaestus is a Golang service used to create container images. This document will guide you through the steps required to set up and run Hephaestus locally.

### Prerequisites
Before starting the setup process, ensure that you have access to the Hephaestus repository, Minikube installed on your system, and have set the environment variables as follows:
`WEBHOOK_SERVER_CERT_DIR=/<your-path-to>/hephaestus/local-development/webhook-certs`

### Setup Process
In the Hephaestus repository, run `go mod tidy` to ensure that all dependencies are installed.
Run the setup scripts available under the scripts/development directory. The scripts need to be run in the following order:
1. `00-sanity-check.sh`: This script asserts that required executables are installed on the host.
2. `01-setup-minikube.sh`: This script creates a Minikube cluster with adequate resources and an extended port range.
3. `02-install-helm-apps.sh`: This script installs required Kubernetes applications and CRDs such as RabbitMQ.
4. `03-expose-buildkit.sh`: This script creates services that target individual BuildKit pods and maps their ingress hostnames so that they mimic the addresses generated from endpoints.
5. `04-export-configurations.sh`: This script exports the controller configuration and webhook TLS files onto the host.

6. In a separate terminal window, run `scripts/development/remote-context/expose.sh`
   - The default image build example in scripts/development/imagebuild.yaml needs to fetch the Dockerfile. This starts a server that runs and serves up all the files from scripts/development/remote-context/.
7. The command to start hephaestus from the root directory: `go run cmd/controller/main.go start --config local-development/hephaestus.yaml`
_If you encounter the error "cannot create connection manager: amqp dial failed," check to make sure that the RabbitMQ connection URL in the hephaestus.yaml file is correct and uses the port._

_Additional runnable commands can be found in the `pkg/cmd/controller/root.go` file._
### 💥 Voila! 💥
Hephaestus is now set up and running locally. From here, the easiest way to create an image build is to run `kubectl create -f scripts/development/imagebuild.yaml`

_If you encounter any trouble or need assistance, please file an issue._
