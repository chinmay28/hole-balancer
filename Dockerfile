# Build a static binary, then ship it on a distroless base. The result is a
# few megabytes and contains nothing but the balancer.
FROM golang:1.24-alpine AS build

ARG VERSION=docker
WORKDIR /src

# Dependencies first, so edits to the source do not invalidate the module
# download layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
        -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/hole-balancer ./cmd/hole-balancer

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/hole-balancer /usr/local/bin/hole-balancer
COPY config.example.yaml /etc/hole-balancer/config.yaml

# Ports 53/udp and 53/tcp for DNS, 8053 for the admin interface. The image
# runs unprivileged, so map the host's port 53 to this high port rather than
# granting the container extra capabilities.
EXPOSE 5300/udp 5300/tcp 8053/tcp

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/hole-balancer"]
CMD ["-config", "/etc/hole-balancer/config.yaml"]
