# syntax=docker/dockerfile:1

FROM docker.io/library/golang:1.26.6-bookworm@sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36 AS build

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

WORKDIR /src

RUN set -eux; \
    test "$TARGETOS" = linux; \
    case "$TARGETARCH" in \
      amd64|arm64) \
        ;; \
      *) \
        echo "unsupported TARGETARCH: $TARGETARCH" >&2; \
        exit 1 \
        ;; \
    esac

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ ./cmd/
COPY internal/ ./internal/

RUN CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build \
      -buildvcs=false \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${BUILD_DATE}" \
      -o /out/ops-pilot \
      ./cmd/ops-pilot

FROM docker.io/library/debian:bookworm-20260623-slim@sha256:60eac759739651111db372c07be67863818726f754804b8707c90979bda511df AS runtime

ARG DEBIAN_SNAPSHOT=20260623T000000Z
ARG GIT_PACKAGE_VERSION=1:2.39.5-0+deb12u3
ARG CA_CERTIFICATES_PACKAGE_VERSION=20230311+deb12u1

RUN set -eux; \
    printf '%s\n' \
      "deb [check-valid-until=no] http://snapshot.debian.org/archive/debian/${DEBIAN_SNAPSHOT} bookworm main" \
      "deb [check-valid-until=no] http://snapshot.debian.org/archive/debian/${DEBIAN_SNAPSHOT} bookworm-updates main" \
      "deb [check-valid-until=no] http://snapshot.debian.org/archive/debian-security/${DEBIAN_SNAPSHOT} bookworm-security main" \
      > /etc/apt/sources.list; \
    rm -f /etc/apt/sources.list.d/debian.sources; \
    apt-get -o Acquire::Check-Valid-Until=false update; \
    apt-get install --yes --no-install-recommends \
      "ca-certificates=${CA_CERTIFICATES_PACKAGE_VERSION}" \
      "git=${GIT_PACKAGE_VERSION}"; \
    test "$(git --version)" = "git version 2.39.5"; \
    test -s /usr/share/doc/git/copyright; \
    rm -rf /var/lib/apt/lists/*; \
    printf '%s\n' 'ops-pilot:x:65532:65532:Ops Pilot:/home/ops-pilot:/usr/sbin/nologin' >> /etc/passwd; \
    printf '%s\n' 'ops-pilot:x:65532:' >> /etc/group; \
    mkdir -p \
      /home/ops-pilot \
      /state/ops-pilot \
      /checkout \
      /cache/ops-pilot/checkouts; \
    chown -R 65532:65532 /home/ops-pilot /state /checkout /cache

COPY --from=build /out/ops-pilot /usr/local/bin/ops-pilot

ENV HOME=/home/ops-pilot \
    USER=ops-pilot \
    XDG_STATE_HOME=/state \
    XDG_CACHE_HOME=/cache

WORKDIR /home/ops-pilot
VOLUME ["/state", "/checkout", "/cache"]
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/ops-pilot"]
