# Stage 1: Build quien binary
FROM golang:1.24-alpine AS build-quien
RUN apk add --no-cache git
RUN GOTOOLCHAIN=auto go install github.com/retlehs/quien@latest

# Stage 2: Build SSH server
FROM golang:1.24-alpine AS build-server
RUN apk add --no-cache git
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /server .

# Stage 3: Minimal runtime
FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY --from=build-quien /go/bin/quien /usr/local/bin/quien
COPY --from=build-server /server /usr/local/bin/server

RUN mkdir -p /app/.ssh
WORKDIR /app

ENV HOST=0.0.0.0
ENV PORT=2222
ENV HOST_KEY_PATH=/app/.ssh/host_key

EXPOSE 2222

CMD ["/usr/local/bin/server"]
