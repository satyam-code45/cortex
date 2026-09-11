# Cortex API — one static binary, no runtime dependencies.
#
# The build is deliberately two stages. The toolchain, module cache and source
# are a few hundred megabytes and none of it is needed to run a Go binary, so
# the final image carries the binary and a CA bundle and nothing else: less to
# pull on every deploy, and no shell or package manager for anything that gets
# in to use.

FROM golang:1.25-alpine AS build

WORKDIR /src

# Manifests first, as their own layer. Source changes far more often than
# dependencies do, so this keeps `go mod download` cached across the edit-build
# cycles that make up almost every deploy.
COPY backend/go.mod backend/go.sum ./
RUN go mod download

COPY backend/ ./

# CGO off because the final image has no libc to link against — the binary must
# be genuinely static or it will not start.
#
# timetzdata embeds the zoneinfo database. Without it time.LoadLocation fails on
# an image that carries no /usr/share/zoneinfo, which surfaces far from its
# cause: timestamps quietly falling back to UTC in something a person reads.
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -tags timetzdata \
        -ldflags="-s -w" \
        -o /out/server ./cmd/server

# static-debian12 carries the CA certificates the outbound calls need — OpenAI,
# Google, Jira, Notion are all HTTPS — and nothing else. nonroot runs as uid
# 65532: the service only ever reads its config and writes to Postgres, so it
# has no reason to hold root in a container.
FROM gcr.io/distroless/static-debian12:nonroot

# 0.0.0.0, not the 127.0.0.1 default. Loopback is right for a local run and
# wrong here: a container that binds loopback is unreachable from outside it, so
# the platform's health check fails and the deploy reads as "never started".
ENV HOST=0.0.0.0

# Documentation only — the platform injects PORT and the server honours it.
EXPOSE 8080

COPY --from=build /out/server /server

ENTRYPOINT ["/server"]
