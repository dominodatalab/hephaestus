FROM golang:1.19-alpine AS build
WORKDIR /app
RUN echo -e 'package main\n\
import "fmt"\n\
func main() {\n\
fmt.Println("hi steve")\n\
}\n' > main.go
RUN CGO_ENABLED=0 go build main.go
RUN cat main.go

FROM gcr.io/distroless/static-debian11
COPY --from=build /app/main /
CMD ["/main"]
