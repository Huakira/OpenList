# Context: an OpenList checkout with drivers/icloud_photos added (see build.sh).
ARG GO_IMAGE=golang:1.26-alpine
ARG OPENLIST_VERSION=v4.2.7
FROM ${GO_IMAGE} AS build
RUN apk add --no-cache gcc musl-dev git
ENV GOTOOLCHAIN=auto CGO_ENABLED=1 CGO_LDFLAGS=-static
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o /openlist -tags=jsoniter \
    -ldflags="-s -w -linkmode external -extldflags '-static -fpic'" .

# Same runtime as the official -aio image; only the binary is swapped.
FROM openlistteam/openlist:${OPENLIST_VERSION}-aio
COPY --from=build --chown=1001:1001 --chmod=755 /openlist /opt/openlist/openlist
