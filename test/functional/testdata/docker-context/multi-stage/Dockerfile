FROM golang:1.23-alpine AS build
WORKDIR /app
RUN printf 'package main\nimport "fmt"\nfunc main() {\nfmt.Println("hi steve")\n}\n' > main.go
RUN CGO_ENABLED=0 go build main.go && \
  cat main.go

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /app/main /
CMD ["/main"]
