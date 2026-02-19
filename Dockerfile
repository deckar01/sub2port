FROM golang:1.26-alpine AS build
WORKDIR /src
COPY main.go .
RUN go mod init sub2port && CGO_ENABLED=0 go build -o /sub2port .

FROM alpine:3.23
COPY --from=build /sub2port /sub2port
ENTRYPOINT ["/sub2port"]
