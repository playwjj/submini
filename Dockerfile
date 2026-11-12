FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
COPY web/ ./web/
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /submini .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY --from=build /submini /usr/local/bin/submini
ENV LISTEN=:9898
EXPOSE 9898
USER 65534:65534
ENTRYPOINT ["/usr/local/bin/submini"]
