# syntax=docker/dockerfile:1
# Multi-stage build: compile in a full Go image, ship only the binary.
# Result: ~15 MB image, no shell, no package manager, runs as non-root.

# ---- build stage ------------------------------------------------------------
FROM golang:1.24-alpine AS build
WORKDIR /src

# Copy dependency manifests first: this layer is cached until go.mod/go.sum
# change, so code edits don't re-download every module.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO_ENABLED=0 -> static binary that runs on a "distroless" base with no libc.
# -trimpath and -s -w strip local paths and debug symbols (smaller, reproducible).
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/simulator ./cmd/simulator

# ---- runtime stage ----------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/simulator /simulator
USER nonroot:nonroot
EXPOSE 2112
ENTRYPOINT ["/simulator"]
