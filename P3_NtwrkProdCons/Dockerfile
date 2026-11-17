# One image for both roles: `consumer` or `producer` is the first argument.
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/p3 ./cmd/p3

FROM debian:bookworm-slim
# ffmpeg makes the 10-second previews and browser-playable copies.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ffmpeg ca-certificates \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=build /out/p3 /usr/local/bin/p3
COPY config.json ./
# 50051 = gRPC uploads, 8080 = web UI (consumer only)
EXPOSE 50051 8080
ENTRYPOINT ["p3"]
CMD ["consumer"]
