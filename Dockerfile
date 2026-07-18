# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/gateway ./cmd/gateway \
 && CGO_ENABLED=0 go build -o /out/mock ./mock

FROM alpine:3.21 AS gateway
COPY --from=build /out/gateway /usr/local/bin/gateway
USER 65532:65532
ENTRYPOINT ["gateway"]

FROM alpine:3.21 AS mock
COPY --from=build /out/mock /usr/local/bin/mock
USER 65532:65532
ENTRYPOINT ["mock"]

# deploy: the live single-instance demo image (Fly.io, see DEPLOY.md). One
# machine runs a co-located plain redis + the echo upstream + the gateway
# via the entrypoint. The gateway/mock stages above are untouched, so
# `make demo` builds byte-identical images.
FROM alpine:3.21 AS deploy
RUN apk add --no-cache redis
COPY --from=build /out/gateway /usr/local/bin/gateway
COPY --from=build /out/mock /usr/local/bin/mock
COPY deploy/entrypoint.sh /usr/local/bin/entrypoint.sh
COPY config.fly.yaml /etc/turnike/config.yaml
RUN chmod 0755 /usr/local/bin/entrypoint.sh
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
