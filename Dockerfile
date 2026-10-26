# syntax=docker/dockerfile:1
# aosc-api: API de la app móvil (AOSC / COVESA). Multi-stage + distroless (plantilla docker-go):
# binario estático sin CGO, CA certs incluidos (llama a Wee, FCM y SMTP por TLS), nonroot, sin shell.
# Config por variables de entorno (env_file); la instancia sale de INSTANCIA.
ARG GO_VERSION=1.27

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/aosc-api ./cmd/aosc-api

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/aosc-api /aosc-api
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/aosc-api"]
