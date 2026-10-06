# syntax=docker/dockerfile:1

# ---- build stage: compile one static binary (web/ and migrations/ are embedded) ----
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# ---- runtime stage: no shell, no package manager, runs as non-root ----
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/server /server
USER nonroot:nonroot
# Render injects PORT at runtime; 8080 is only the local default.
ENV PORT=8080
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=90s --retries=3 CMD ["/server", "healthcheck"]
ENTRYPOINT ["/server"]
