FROM golang:1.18-alpine AS build
WORKDIR /app
COPY go.mod .
COPY go.sum .
RUN go mod download
COPY cmd ./cmd
COPY pkg ./pkg
COPY deployments/crds ./deployments/crds
ENV CGO_ENABLED=0 GOOS=linux
RUN go build -o hephaestus-controller ./cmd/controller

FROM gcr.io/distroless/static:debug
WORKDIR /
COPY --from=build /app/hephaestus-controller /usr/bin/
ENTRYPOINT ["hephaestus-controller"]
