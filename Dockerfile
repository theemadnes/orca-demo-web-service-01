# Build stage
FROM golang:1.26-alpine AS builder

# Install ca-certificates in the builder stage so we can copy them to scratch
RUN apk --no-cache add ca-certificates

WORKDIR /app

# Copy go mod and sum files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY main.go ./

# Build the application
# CGO_ENABLED=0 creates a statically-linked binary
# -ldflags="-s -w" strips debug information and symbol tables to reduce size
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o orca-demo main.go

# Run stage
FROM scratch

# Copy CA certificates from builder for any HTTPS needs
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Copy the binary from the builder stage
COPY --from=builder /app/orca-demo /orca-demo

# Expose port 8080
EXPOSE 8080

# Run the binary
ENTRYPOINT ["/orca-demo"]
