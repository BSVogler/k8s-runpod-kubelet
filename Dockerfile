FROM golang AS builder

WORKDIR /app

# Copy go mod and sum files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy the source code
COPY . .

# Build the application
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o virtual_kubelet ./cmd/virtual_kubelet

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /app/runpod_controller .
USER 65532:65532

ENTRYPOINT ["/runpod_controller"]