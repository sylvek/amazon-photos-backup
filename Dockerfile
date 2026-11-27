# The build stage runs on the builder's own architecture and cross-compiles,
# so multi-platform images need no emulation.
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY main.go ./
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/amazon-photos-backup . \
 && mkdir -p /out/data/photos /out/data/state

# Static binary on a minimal base: CA certificates and time zones, no shell.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/amazon-photos-backup /usr/local/bin/amazon-photos-backup
COPY --from=build --chown=65532:65532 /out/data /data
VOLUME /data
USER 65532:65532
HEALTHCHECK --interval=5m --timeout=10s --start-period=1m --retries=1 \
  CMD ["/usr/local/bin/amazon-photos-backup", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/amazon-photos-backup"]
CMD ["run"]
