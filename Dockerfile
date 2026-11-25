FROM --platform=$BUILDPLATFORM golang:1.27 AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /molt ./cmd/molt

# Gate loads a native library at runtime (vialite, via purego): it needs a C library.
FROM gcr.io/distroless/base-debian12:nonroot
COPY --from=build /molt /molt
ENTRYPOINT ["/molt"]
