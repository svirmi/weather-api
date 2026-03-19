FROM golang:1.26.0-alpine AS golang

WORKDIR /app
COPY . .

RUN go mod download
RUN go mod verify

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /proxy .

FROM gcr.io/distroless/static-debian11

COPY --from=golang /proxy .

EXPOSE 8080

CMD ["/proxy"]