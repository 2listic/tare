# tare-agent image: a static Go binary on a distroless base (no shell, no package manager).
#   docker build -t tare-agent:poc agent/
# The build stage runs on the builder's own platform and cross-compiles, so a
# multi-arch build (.github/workflows/agent-image.yml) needs no emulation.
FROM --platform=$BUILDPLATFORM golang:1.27.3 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
ARG VERSION=poc
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /tare-agent .

FROM gcr.io/distroless/static-debian12
COPY --from=build /tare-agent /tare-agent
ENTRYPOINT ["/tare-agent"]
