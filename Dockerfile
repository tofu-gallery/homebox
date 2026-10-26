# Node dependencies stage
FROM mirror.gcr.io/library/node:22-alpine AS frontend-dependencies
WORKDIR /app

# Install pnpm 10 (latest stable, works reliably in Alpine)
RUN npm install -g pnpm@10

# Copy package.json and lockfile to leverage caching
COPY frontend/package.json frontend/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile

# Build Nuxt (frontend) stage
FROM mirror.gcr.io/library/node:22-alpine AS frontend-builder
WORKDIR /app

# Install pnpm 10 (latest stable)
RUN npm install -g pnpm@10

# Copy over source files and node_modules from dependencies stage
COPY frontend . 
COPY --from=frontend-dependencies /app/node_modules ./node_modules
RUN pnpm build

# Go dependencies stage
FROM mirror.gcr.io/library/golang:alpine AS builder-dependencies
WORKDIR /go/src/app

# Copy go.mod and go.sum for better caching
COPY ./backend/go.mod ./backend/go.sum ./
RUN go mod download

# Build API stage
FROM mirror.gcr.io/library/golang:alpine AS builder
ARG TARGETOS
ARG TARGETARCH
ARG BUILD_TIME
ARG COMMIT
ARG VERSION

# Install necessary build tools
RUN apk update && \
    apk upgrade && \
    apk add --no-cache git build-base gcc g++ && \
    if [ "$TARGETARCH" != "arm" ] || [ "$TARGETARCH" != "riscv64" ]; then apk --no-cache add libwebp libavif libheif libjxl; fi

WORKDIR /go/src/app

# Copy Go modules (from dependencies stage) and source code
COPY --from=builder-dependencies /go/pkg/mod /go/pkg/mod
COPY ./backend .

# Clear old public files and copy new ones from frontend build
RUN rm -rf ./app/api/public
COPY --from=frontend-builder /app/.output/public ./app/api/static/public

# Use cache for Go build artifacts
RUN --mount=type=cache,target=/root/.cache/go-build \
    if [ "$TARGETARCH" = "arm" ] || [ "$TARGETARCH" = "riscv64" ];  \
    then echo "nodynamic" $TARGETOS $TARGETARCH; CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
        -ldflags "-s -w -X main.commit=$COMMIT -X main.buildTime=$BUILD_TIME -X main.version=$VERSION" \
        -tags nodynamic -o /go/bin/api -v ./app/api/*.go; \
    else \
         echo $TARGETOS $TARGETARCH; CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
        -ldflags "-s -w -X main.commit=$COMMIT -X main.buildTime=$BUILD_TIME -X main.version=$VERSION" \
        -o /go/bin/api -v ./app/api/*.go; \
    fi

# Production stage
FROM mirror.gcr.io/library/alpine:latest
ENV HBOX_MODE=production
ENV HBOX_STORAGE_CONN_STRING=file:///?no_tmp_dir=true
ENV HBOX_STORAGE_PREFIX_PATH=data
ENV HBOX_DATABASE_SQLITE_PATH=/data/homebox.db?_pragma=busy_timeout=2000&_pragma=journal_mode=WAL&_fk=1&_time_format=sqlite
# Tofu (2026-10-23): thumbnails and the hourly GitHub release check are off by default on Tofu
ENV HBOX_THUMBNAIL_ENABLED=false
ENV HBOX_OPTIONS_GITHUB_RELEASE_CHECK=false
# Tofu (2026-10-23): the sign-in page and footer link to the exact source this image is built from (AGPL-3.0 section 13)
# Tofu (2026-10-26): the address names this pin's own tag
ENV HBOX_OPTIONS_SOURCE_URL=https://github.com/tofu-gallery/homebox/tree/pin/2026-10-26.1

# Install necessary runtime dependencies
RUN apk --no-cache add ca-certificates wget mosquitto-clients && \
    if [ "$TARGETARCH" != "arm" ] || [ "$TARGETARCH" != "riscv64" ]; then apk --no-cache add libwebp libavif libheif libjxl; fi

# Create application directory and copy over built Go binary
RUN mkdir /app
COPY --from=builder /go/bin/api /app
RUN chmod +x /app/api
# Tofu (2026-10-23): maps Tofu's PORT and DATABASE_URL onto HBOX_* settings before starting /app/api
COPY tofu-entrypoint.sh /app/tofu-entrypoint.sh

# Labels and configuration for the final image
LABEL Name=homebox Version=0.0.1
LABEL org.opencontainers.image.source="https://github.com/sysadminsmedia/homebox"

# Expose necessary ports for Homebox
EXPOSE 7745
WORKDIR /app

# Healthcheck configuration
# Tofu (2026-10-23): checks the port the server listens on (PORT, then HBOX_WEB_PORT, then 7745)
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 -O - "http://localhost:${PORT:-${HBOX_WEB_PORT:-7745}}/api/v1/status" || exit 1

# Persist volume
VOLUME [ "/data" ]

# Entrypoint and CMD
# Tofu (2026-10-23): starts through tofu-entrypoint.sh, which ends by running /app/api with the same arguments
ENTRYPOINT [ "/bin/sh", "/app/tofu-entrypoint.sh" ]
CMD [ "/data/config.yml" ]
