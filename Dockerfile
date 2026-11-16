# syntax=docker/dockerfile:1.7
FROM golang:1.27-alpine AS build
ARG VERSION=dev
ARG COMMIT=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X github.com/mrfroglab/sewain-api/internal/platform/health.Version=${VERSION} -X github.com/mrfroglab/sewain-api/internal/platform/health.Commit=${COMMIT}" \
      -o /out/api ./cmd/api \
    && mkdir -p /out/data/uploads

FROM gcr.io/distroless/static-debian12:nonroot
ARG VERSION=dev
ARG COMMIT=unknown
LABEL org.opencontainers.image.title="sewain-api" \
      org.opencontainers.image.description="Sewain API (Go + Postgres/PostGIS)" \
      org.opencontainers.image.source="https://github.com/mrfroglab/sewain-api" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}"
WORKDIR /app
COPY --from=build /out/api /app/api
COPY --from=build --chown=nonroot:nonroot /out/data /data
ENV PORT=8080 APP_ENV=demo UPLOAD_DIR=/data/uploads
EXPOSE 8080
USER nonroot:nonroot
HEALTHCHECK --interval=15s --timeout=4s --start-period=20s --retries=5 CMD ["/app/api", "healthcheck"]
ENTRYPOINT ["/app/api"]
