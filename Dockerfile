FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
COPY web/ ./web/
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /submini .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates \
 && mkdir -p /data && chown 65534:65534 /data
COPY --from=build /submini /usr/local/bin/submini
ENV LISTEN=:9898 DATA_DIR=/data
EXPOSE 9898
VOLUME /data
USER 65534:65534
ENTRYPOINT ["/usr/local/bin/submini"]
