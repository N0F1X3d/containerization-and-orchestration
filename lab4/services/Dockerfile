# Один Dockerfile на оба сервиса: APP=api или APP=worker.
#   docker build --build-arg APP=api --build-arg VERSION=1.0 -t registry.lab.local/shop/api:1.0 .
FROM golang:1.26-alpine AS build
ARG APP
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=${VERSION}" -o /app ./cmd/${APP}

FROM scratch
COPY --from=build /app /app
USER 65534:65534
EXPOSE 8080
ENTRYPOINT ["/app"]
