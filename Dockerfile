# Go services: api, worker, migrate, newsctl in one image (select with the container command).
# Behind a TLS-inspecting proxy: docker build --secret id=extra_ca,src=/path/ca.pem .
FROM golang:1.26-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
RUN --mount=type=secret,id=extra_ca,required=false \
    if [ -s /run/secrets/extra_ca ]; then cat /run/secrets/extra_ca >> /etc/ssl/certs/ca-certificates.crt; fi
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    for c in api worker migrate newsctl; do go build -ldflags="-s -w" -o /out/$c ./cmd/$c; done

FROM alpine:3.21
# No package installs: CA roots come from the build stage; busybox provides wget for healthchecks.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
RUN adduser -D -u 10001 app
WORKDIR /app
COPY --from=build /out/ /usr/local/bin/
COPY fixtures ./fixtures
COPY config ./config
COPY scripts/init.sh ./scripts/init.sh
ENV FIXTURE_DIR=/app/fixtures STORAGE_DIR=/app/var/storage
RUN mkdir -p /app/var/storage && chown -R app /app/var
USER app
EXPOSE 8080 9090 9091
CMD ["api"]
