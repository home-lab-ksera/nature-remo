FROM golang:1.26-alpine@sha256:a2012b4a1d1f9f7e9f44e0d39b805a901e856b1dd80b4e6ee4338db547050308 AS builder
WORKDIR /src
COPY go.mod ./
COPY main.go ./
COPY web ./web
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/nature-remo .
RUN mkdir -p /out/data && chown 1000:1000 /out/data

FROM scratch
COPY --from=builder /out/nature-remo /nature-remo
COPY --from=builder --chown=1000:1000 /out/data /data
USER 1000:1000
ENV DATA_DIR=/data LISTEN_ADDR=:8080
EXPOSE 8080
ENTRYPOINT ["/nature-remo"]
