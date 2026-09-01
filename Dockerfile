# Build the server image.
#
# Two stages, so the shipped image holds a binary and nothing else: no compiler,
# no module cache, no source. That is a smaller attack surface as much as a
# smaller download — a build toolchain inside a running container is a set of
# capabilities an attacker who gets in would otherwise inherit.

FROM golang:1.25-alpine AS build
WORKDIR /src

# Dependencies first, as their own layer. They change far less often than the
# code, so an ordinary edit reuses this layer instead of re-downloading the
# module graph on every build.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 for a static binary, so the runtime stage can be a distroless
# image with no libc at all.
#
# The frontend bundle under internal/web/dist is committed and embedded by
# go:embed, so this build needs no Node. That is the same property that lets a
# contributor run `go build` without a JavaScript toolchain installed.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/trace-portal ./cmd/trace-portal

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/trace-portal /trace-portal

# Non-root by default. The server writes only to its data volume, so there is
# nothing it needs root for, and the day it is compromised is the day this
# matters.
USER nonroot:nonroot

# 0.0.0.0 inside a container is loopback-equivalent from the host's point of
# view: nothing reaches it except what the runtime publishes. Binding to
# 127.0.0.1 here would make the server unreachable from outside its own
# namespace, which looks like a hang rather than a configuration mistake.
EXPOSE 8317
VOLUME ["/data"]

ENTRYPOINT ["/trace-portal"]
CMD ["-addr", "0.0.0.0:8317", "-data", "/data"]
