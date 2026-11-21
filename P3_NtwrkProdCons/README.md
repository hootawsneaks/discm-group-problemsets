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

## Configuration

Everything is set in `config.json`; the only override is the `CONSUMER_ADDR` environment variable, which replaces `consumer_addr` (used by `docker-compose.yml`). The main settings:

- `producers` (p): number of producer threads. Thread i uploads every video in `videos/p<i>/`, so create `videos/p1` to `videos/p<p>`.
- `consumers` (c): number of consumer threads.
- `max_queue` (q): queue length; uploads beyond it are dropped.
- `consumer_addr`: where the producer sends uploads (`host:port`).

The producer waits up to `connect_wait` for the consumer, so start order doesn't matter.

## Run locally

Run the consumer and producer in two separate terminals.

Linux / macOS:
```sh
go build -o bin/p3 ./cmd/p3
bin/p3 consumer    # gRPC on :50051, GUI at http://localhost:8080
bin/p3 producer
```

Windows PowerShell (the binary needs the `.exe` extension):
```powershell
go build -o bin/p3.exe ./cmd/p3
.\bin\p3.exe consumer    # gRPC on :50051, GUI at http://localhost:8080
.\bin\p3.exe producer
```

Use `-config <path>` to load a different config file.

## Run on two VMs

On both VMs, clone the repo and build the image:
```sh
docker build -t p3 .
```

Consumer VM (open TCP 50051 and 8080 in its firewall):

Linux / macOS:
```sh
docker run -p 50051:50051 -p 8080:8080 \
  -v $PWD/config.json:/app/config.json:ro -v $PWD/data:/app/data p3 consumer
```

Windows PowerShell:
```powershell
docker run -p 50051:50051 -p 8080:8080 -v ${PWD}/config.json:/app/config.json:ro -v ${PWD}/data:/app/data p3 consumer
```

Producer VM: first set `"consumer_addr": "<consumer-vm-ip>:50051"` in its `config.json`, then:

Linux / macOS:
```sh
docker run -v $PWD/config.json:/app/config.json:ro -v $PWD/videos:/app/videos:ro p3 producer
```

Windows PowerShell:
```powershell
docker run -v ${PWD}/config.json:/app/config.json:ro -v ${PWD}/videos:/app/videos:ro p3 producer
```

`config.json` is mounted into the container, so edits take effect on the next run without rebuilding.

## Run both on one machine

`docker-compose.yml` starts the consumer and producer as two separate containers:

```sh
docker compose up --build    # build and start both containers
docker compose down          # stop and remove them
```

- **consumer**: gRPC on `50051`, GUI at http://localhost:8080; uploads are saved to `./data`.
- **producer**: reads `./videos` (read-only).
- IP addresses are declared in `docker-compose.yml`, not auto-assigned: consumer `172.28.0.10`, producer `172.28.0.11` on the `p3net` subnet (`172.28.0.0/16`). The producer gets `CONSUMER_ADDR=172.28.0.10:50051`, which overrides `consumer_addr` in `config.json`.

Both containers read `./config.json`. After editing it, run `docker compose up` again.
