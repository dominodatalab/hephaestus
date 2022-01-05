![hephaestus-logo.png](assets/logo.png)

# Hephaestus

Secure and performant OCI-image builder for Kubernetes.

## POC Quick Setup

```shell
# required tools
brew install helm kind stern

# create local K8s cluster
kind create cluster

# build and load project image
make docker
kind load docker-image ghcr.io/dominodatalab/hephaestus:latest

# deploy hephaestus & buildkitd
helm upgrade -i hephaestus deployments/helm/hephaestus

# YOU WILL WANT TO MODIFY THE EXAMPLES PRIOR TO APPLYING THEM

# cache image layers on all buildkitd workers
kubectl apply -f examples/resources/imagecache.yaml

# run an image build
kubectl apply -f examples/resources/imagebuild.yaml

# inspect the build logs
stern -l app.kubernetes.io/name=hephaestus
```
