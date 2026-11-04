# The control plane: one static binary plus the docker CLI.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY web ./web
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /claudia .

FROM alpine:3.22
RUN apk add --no-cache ca-certificates docker-cli
COPY --from=build /claudia /usr/local/bin/claudia
VOLUME /data
EXPOSE 8080 8081
ENTRYPOINT ["claudia"]
