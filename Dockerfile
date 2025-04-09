FROM golang:1.24-alpine AS build
ARG VERSION=dev
ENV VERSION=${VERSION}
WORKDIR /app
COPY go.mod .
COPY go.sum .
RUN go mod download
COPY cmd ./cmd
COPY pkg ./pkg
COPY deployments/crds ./deployments/crds
ENV CGO_ENABLED=0 GOOS=linux
RUN go build -ldflags="-X 'main.Version=${VERSION}'" -o hephaestus-controller ./cmd/controller

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=build /app/hephaestus-controller /usr/bin/
ENTRYPOINT ["hephaestus-controller"]
