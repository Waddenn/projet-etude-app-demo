FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -trimpath -o /out/api          ./cmd/api && \
    CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -trimpath -o /out/worker       ./cmd/worker && \
    CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -trimpath -o /out/audit-purge  ./cmd/audit-purge

# Image API (cible par défaut pour compatibilité avec l'existant)
FROM gcr.io/distroless/static-debian12:nonroot AS api
COPY --from=build /out/api /app
USER nonroot
EXPOSE 8080
ENTRYPOINT ["/app"]

# Image worker
FROM gcr.io/distroless/static-debian12:nonroot AS worker
COPY --from=build /out/worker /app
USER nonroot
EXPOSE 8081
ENTRYPOINT ["/app"]

# Image utilitaire audit-purge (CronJob)
FROM gcr.io/distroless/static-debian12:nonroot AS audit-purge
COPY --from=build /out/audit-purge /app
USER nonroot
ENTRYPOINT ["/app"]
