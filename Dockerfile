# Build stage
FROM cgr.dev/chainguard/go:latest AS builder

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY internal internal
COPY cmd cmd
COPY main.go .

# Build the binary
RUN CGO_ENABLED=0 go build -o container-image-exporter .

# Final stage
FROM cgr.dev/chainguard/static:latest

COPY --from=builder /app/container-image-exporter /container-image-exporter

# Expose the default metrics and health probe ports used by the controller
# subcommand. The node-exporter subcommand uses only 8080.
EXPOSE 8080 8081

ENTRYPOINT ["/container-image-exporter"]
