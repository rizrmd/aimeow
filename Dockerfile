# Build stage
FROM golang:1.25-alpine AS builder

# Install build dependencies
RUN apk add --no-cache git ca-certificates tzdata gcc musl-dev sqlite-dev

# Set working directory
WORKDIR /app

# Copy go mod and sum files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Create docs directory (if it doesn't exist in git)
RUN mkdir -p docs

# Build the application
RUN CGO_ENABLED=1 GOOS=linux go build -a -installsuffix cgo -o aimeow .

# Final stage
FROM alpine:latest

# Install runtime dependencies
RUN apk --no-cache add ca-certificates sqlite wget

# Create non-root user
RUN addgroup -g 1001 -S appgroup && \
    adduser -u 1001 -S appuser -G appgroup

# Set working directory
WORKDIR /app

# Copy binary from builder stage
COPY --from=builder /app/aimeow .

# Copy documentation files
COPY --from=builder /app/docs ./docs

# Create files directory with proper permissions
RUN mkdir -p /app/files && \
    chown -R appuser:appgroup /app

# Volume mount for persistent data
VOLUME ["/app/files"]

# Switch to non-root user
USER appuser

# Expose port
EXPOSE 7030

# Health check
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:7030/health || exit 1

# Run the application
CMD ["./aimeow"]