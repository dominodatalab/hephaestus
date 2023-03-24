
### Introduction

Hephaestus, also known as the v3 ImageBuilder, is a Golang service used to create container images. This document will guide you through the steps required to set up and run Hephaestus locally.

### Prerequisites
Before starting the setup process, ensure that you have access to the Hephaestus repository, Minikube installed on your system, and have set the environment variables as follows:
`WEBHOOK_SERVER_CERT_DIR=/<your-path-to>/hephaestus/local-development/webhook-certs`

### Setup Process
In the Hephaestus repository, run go mod tidy to ensure that all dependencies are installed.
Set the necessary webhook environment variable by adding the following program arguments: start -c local-development/hephaestus.yaml.
Run the setup scripts available under the scripts/development directory. The scripts need to be run in the following order:
1. `00-sanity-check.sh`: This script asserts that required executables are installed on the host.
2. `01-setup-minikube.sh`: This script creates a Minikube cluster with adequate resources and an extended port range. Note that if you encounter the error "GUEST_DRIVER_MISMATCH," remove the --driver=hyperkit argument on line 16 in the Minikube start command.
3. `02-install-helm-apps.sh`: This script installs required Kubernetes applications and CRDs such as RabbitMQ.
4. `03-expose-buildkit.sh`: This script creates services that target individual BuildKit pods and maps their ingress hostnames so that they mimic the addresses generated from endpoints.
5. `04-export-configurations.sh`: This script exports the controller configuration and webhook TLS files onto the host.
6. Update your `local-development/hephaestus.yaml` file to the correct RabbitMQ port by running `minikube service rabbitmq` in a new terminal window.
This command exposes the RabbitMQ service to your local machine and allows you to interact with it as if it were running on your local machine.
After running this command, there will be a list towards the bottom of the command with available ports.
```
[default rabbitmq  http://127.0.0.1:50377
http://127.0.0.1:53975
http://127.0.0.1:53976
http://127.0.0.1:53977]
```
Get the first port in the default service list _here it's 50377_,  update that port in two places. Examples in the code blocks below.
- `local-development/hephaestus.yaml` on lines 36
- `scripts/development/helmfile.yaml` on line 17.

_local-development/hephaestus.yaml L36_
```
amqp:
url: "amqp://user:roger-rabbit@localhost:50377"
```

_scripts/development/helmfile.yaml on L17_
```
releases:
- name: rabbitmq
  namespace: default
  chart: bitnami/rabbitmq
  version: 11.1.1
  values:
    - auth:
      password: roger-rabbit
      service:
      type: NodePort
      nodePorts:
      amqp: 50377
```

7. In a separate terminal window, run `scripts/development/remote-context/expose.sh`
8. The command to start hephaestus from the root directory: `go run cmd/controller/main.go start --config local-development/hephaestus.yaml`
_If you encounter the error "cannot create connection manager: amqp dial failed," check to make sure that the RabbitMQ connection URL in the hephaestus.yaml file is correct and uses the port created when you ran minikube service rabbitmq, pointing back to localhost._

_Additional runnable commands can be found in the `pkg/cmd/controller/root.go` file._
### 💥 Viola! 💥
Hephaestus is now set up and running locally. From here, the easiest way to create an image build is to run `kubectl create -f scripts/development/imagebuild.yaml`

_If you encounter any trouble or need assistance, please reach out to the `#platform-help` channel._



