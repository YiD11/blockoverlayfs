# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETARCH
# The build context carries no git history to describe the version from.
ARG VERSION=unknown
ARG REVISION=unknown
RUN make binaries GOARCH=$TARGETARCH VERSION=$VERSION REVISION=$REVISION

FROM alpine:3.22
# mkfs.ext4 formats the images of the block layers.
RUN apk add --no-cache e2fsprogs
COPY --from=build /src/bin/blockoverlayfs /usr/local/bin/blockoverlayfs
ENTRYPOINT ["/usr/local/bin/blockoverlayfs"]
