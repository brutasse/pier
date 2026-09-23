# syntax=docker/dockerfile:1

FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/pier ./cmd/pier

FROM gcr.io/distroless/static-debian12:nonroot
# TLS to S3 and to the IdP needs CA certificates.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/pier /pier
EXPOSE 8080
ENTRYPOINT ["/pier"]
CMD ["-config", "/config/pier.yaml"]
