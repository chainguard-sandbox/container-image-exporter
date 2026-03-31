# Build stage
FROM cgr.dev/chainguard/go:latest AS builder

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY internal internal
COPY main.go .

# Build the binary
RUN CGO_ENABLED=0 go build -o app .

# Final stage
FROM cgr.dev/chainguard/static:latest

# Copy the binary from builder stage
COPY --from=builder /app/app /app

# Expose the default ports
EXPOSE 8080 8081

# Run the binary
ENTRYPOINT ["/app"]
