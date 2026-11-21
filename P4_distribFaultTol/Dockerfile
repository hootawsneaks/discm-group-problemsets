# One image for every node; the role is the first argument: view | auth | course | grade | announce
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/p4 ./cmd/p4

FROM debian:bookworm-slim
WORKDIR /app
COPY --from=build /out/p4 /usr/local/bin/p4
COPY config.json ./
# 8080 = web (view), 50051 = auth, 50052 = course, 50053 = grade, 50054 = announce
EXPOSE 8080 50051 50052 50053 50054
ENTRYPOINT ["p4"]
CMD ["view"]
