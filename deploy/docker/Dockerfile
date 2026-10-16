# A local Aries test server: ariesd, and sshd with SSH on, in one container, set up as
# docs/server.md sets up a machine, beside a Postgres container that
# compose.yaml runs. For testing only, never for production. Build from
# the repository root, so the image holds whatever branch is checked
# out; docs/deploy/docker.md has the commands.
#
# Behind a TLS-intercepting proxy, pass its CA as a build secret, which is
# never stored in the image: --secret id=ca,src=/path/to/ca.pem

# golang:1.27.2-trixie
FROM --platform=$BUILDPLATFORM golang:1.27-trixie@sha256:2f84bc93ecfb2689f782b153fdcd368b5a7ab96c1386c65cdaccf35e726d6a44 AS build
ENV GOTOOLCHAIN=local
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=secret,id=ca,required=false \
    if [ -f /run/secrets/ca ]; then export SSL_CERT_FILE=/run/secrets/ca; fi; \
    go mod download && go mod verify
COPY cmd cmd
COPY internal internal
ARG TARGETOS TARGETARCH
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X github.com/ariesworx/aries/internal/version.Version=${VERSION}" \
      -o /out/ ./cmd/ariesd ./cmd/aries

# ubuntu:24.04.5, the release docs/server.md's steps were tested on
FROM ubuntu:24.04@sha256:534baea6a22c03a63003dbc8dbe78fe34bc0d7e595d9a9dc9834884ff530eb55
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends openssh-server openssl \
 && rm -rf /var/lib/apt/lists/* \
 && rm -f /etc/ssh/ssh_host_*
COPY --from=build /out/ariesd /out/aries /usr/local/bin/

# Every piece of state lives on the /state volume. The paths docs/server.md
# names link into it, so its commands work here unchanged. A fixed id
# keeps the volume's files owned by the same account after a rebuild.
RUN groupadd --system --gid 901 aries \
 && useradd --system --uid 901 --gid aries --create-home --home-dir /var/lib/aries --shell /bin/sh aries \
 && usermod -p '*' aries \
 && install -d -m 0755 /state \
 && ln -s /state/aries /etc/aries \
 && ln -s /state/ssh /var/lib/aries/.ssh
COPY deploy/docker/sshd.conf /etc/ssh/sshd_config.d/00-aries.conf
COPY deploy/docker/entrypoint.sh /usr/local/bin/aries-entrypoint
VOLUME /state
EXPOSE 22 8443
HEALTHCHECK --start-period=60s --start-interval=1s CMD ["test", "-e", "/run/aries-ready"]
ENTRYPOINT ["/usr/local/bin/aries-entrypoint"]
