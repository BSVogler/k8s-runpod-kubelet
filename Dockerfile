# syntax=docker/dockerfile:1

# Build stage. TARGETOS/TARGETARCH are set by buildx for multi-arch builds.
FROM --platform=$BUILDPLATFORM golang:1.24 AS builder

ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=dev

WORKDIR /src

# Cache module downloads separately from the source
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/conduit-kubelet ./cmd/virtual_kubelet

# Runtime stage: static, no shell, non-root (uid 65532)
FROM gcr.io/distroless/static:nonroot

LABEL org.opencontainers.image.source="https://github.com/BSVogler/k8s-runpod-kubelet" \
      org.opencontainers.image.description="Conduit Kubelet: virtual kubelet that connects a Kubernetes cluster to the Conduit GPU platform" \
      org.opencontainers.image.licenses="PolyForm-Strict-1.0.0"

WORKDIR /
COPY --from=builder /out/conduit-kubelet /conduit-kubelet
USER 65532:65532

EXPOSE 8080 10250

ENTRYPOINT ["/conduit-kubelet"]
