FROM --platform=$BUILDPLATFORM golang:1.26.10-alpine AS build
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY pkg ./pkg
COPY deployments/crds ./deployments/crds
# Cross-compile natively using Go's built-in support (no QEMU emulation needed)
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -ldflags="-X 'main.Version=${VERSION}'" -o hephaestus-controller ./cmd/controller

FROM cgr.dev/chainguard/static:latest@sha256:fc42af33e643f20d08baf03cddcc1c02ba57052dfb02edb0d79f3fc47bcea8d9
WORKDIR /
COPY --from=build /app/hephaestus-controller /usr/bin/
ENTRYPOINT ["hephaestus-controller"]
