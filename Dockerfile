FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/collector ./cmd/collector

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/collector /collector
EXPOSE 9090
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
  CMD ["/collector","healthcheck"]
ENTRYPOINT ["/collector"]
