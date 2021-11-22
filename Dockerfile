ARG command=controller

FROM golang:1.17-alpine AS build
WORKDIR /app
COPY go.mod .
COPY go.sum .
RUN go mod download
COPY cmd ./cmd
COPY pkg ./pkg
ARG command
RUN CGO_ENABLED=0 go build -o hephaestus-$command ./cmd/$command/

FROM gcr.io/distroless/static:nonroot
WORKDIR /
ARG command
COPY --from=build /app/hephaestus-$command /
ENTRYPOINT ["/hephaestus-$command"]
