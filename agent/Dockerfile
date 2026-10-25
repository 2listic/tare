# tare-agent image: a static Go binary on a distroless base (no shell, no package manager).
#   docker build -t tare-agent:poc agent/
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
ARG VERSION=poc
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /tare-agent .

FROM gcr.io/distroless/static-debian12
COPY --from=build /tare-agent /tare-agent
ENTRYPOINT ["/tare-agent"]
