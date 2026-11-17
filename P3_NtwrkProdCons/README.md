# P3 – Networked Producer/Consumer (gRPC video upload)

```
cmd/p3/                 entry point: `p3 consumer` or `p3 producer`
internal/config/        config.json loading + defaults
internal/consumer/      gRPC upload server, leaky-bucket queue, c worker threads, web UI
internal/consumer/web/  the GUI (embedded into the binary)
internal/producer/      p producer threads, each uploading videos/p<i>/
proto/                  upload.proto + generated code
docs/                   notes
```

Runtime folders (git-ignored): `videos/p1, videos/p2, ...` (producer input), `data/` (consumer output), `bin/`.

## Run locally

```sh
go build -o bin/p3 ./cmd/p3
bin/p3 consumer -c 2 -q 5          # gRPC on :50051, GUI at http://localhost:8080
bin/p3 producer -p 3               # producer i uploads every video in videos/p<i>/
```

Flags override `config.json`: `-p -c -q`, consumer `-listen -http`, producer `-addr -input -wait`.
The producer waits up to `-wait` (default 60s) for the consumer, so start order doesn't matter.

## Run on two VMs

Consumer VM (open TCP 50051 and 8080 in its firewall):
```sh
docker build -t p3 .
docker run -p 50051:50051 -p 8080:8080 -v $PWD/data:/app/data p3 consumer -c 2 -q 5
```

Producer VM:
```sh
docker build -t p3 .
docker run -v $PWD/videos:/app/videos:ro p3 producer -p 3 -addr <consumer-vm-ip>:50051
```

Or both on one machine: `docker compose up --build`.
