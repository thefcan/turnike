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
