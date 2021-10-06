# Build Arguments
ARG BUILDER_IMAGE=golang:buster
ARG FINAL_IMAGE=gcr.io/distroless/static

# Build Stage
FROM ${BUILDER_IMAGE} AS builder

# Ensure ca-certificates are up to date
RUN update-ca-certificates

WORKDIR $GOPATH/src/github.com/bbengfort/keepalive

# Use modules for dependencies
COPY go.mod .
COPY go.sum .

ENV GO111MODULE=on
RUN go mod download
RUN go mod verify

COPY . .

# Run tests
RUN CGO_ENABLED=0 go test -timeout 30s -v ./...

# Build the static binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
        -ldflags='-w -s -extldflags "static"' -a \
        -o /go/bin/keepalive ./cmd/keepalive

# Final Stage
FROM ${FINAL_IMAGE} AS final

LABEL maintainer="Benjamin Bengfort <benjamin@bengfort.com>"
LABEL description="An experiment to see what it takes to keep a gRPC stream alive indefinitely"

# Use nonroot user for security
USER nonroot:nonroot

# Copy compiled static app from builder
COPY --from=builder --chown=nonroot:nonroot /go/bin/keepalive /bin/keepalive

# Run binary; use vector form
ENTRYPOINT [ "/bin/keepalive", "serve" ]