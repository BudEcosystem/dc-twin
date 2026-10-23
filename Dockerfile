# dc-twin: the dctwin binary (the packs and stacks are built into it), the console and the
# example estates. The assets the packs obtain (captured BMC trees, NVIDIA's NMX-C interface
# definition, the os: twin guest image) are not in the image: some may not be redistributed (D13),
# so `dctwin assets fetch` obtains them where the twin runs, as the chart's init container does.
#
#   docker build -t ghcr.io/budecosystem/dc-twin:dev .
#   docker buildx build --platform linux/amd64,linux/arm64 -t <registry>/dc-twin:<tag> --push .

FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd cmd
COPY internal internal
COPY packs packs
COPY stacks stacks
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/dctwin ./cmd/dctwin
# the state and assets directories, owned by the nonroot user the image runs as
RUN mkdir -p /out/var/lib/dctwin/state /out/var/lib/dctwin/assets

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/dctwin /usr/local/bin/dctwin
COPY --from=build --chown=65532:65532 /out/var/lib/dctwin /var/lib/dctwin
COPY console /opt/dctwin/console
COPY examples /opt/dctwin/examples
COPY LICENSE /opt/dctwin/LICENSE
ENV DCTWIN_STATE=/var/lib/dctwin/state DCTWIN_ASSETS=/var/lib/dctwin/assets
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/dctwin"]
CMD ["help"]
