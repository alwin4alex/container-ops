# syntax=docker/dockerfile:1
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/container-ops ./cmd/container-ops

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/container-ops /container-ops
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/container-ops"]
CMD ["-config", "/etc/container-ops/config.yaml"]
