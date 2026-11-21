# P3 – Networked Producer/Consumer (gRPC video upload)

```
cmd/p3/                 entry point: `p3 consumer` or `p3 producer`
internal/config/        config.json loading + defaults
internal/consumer/      gRPC upload server, leaky-bucket queue, c worker threads, web UI
internal/consumer/web/  the GUI (embedded into the binary)
internal/producer/      p producer threads, each uploading videos/p<i>/
internal/tlsgen/        self-signed certificate for the HTTPS GUI
proto/                  upload.proto + generated code
```

Runtime folders (git-ignored): `videos/p1, videos/p2, ...` (producer input), `data/` (consumer
output), `certs/` (generated certificate), `bin/`.

## Configuration

Everything is set in `config.json`; the only override is the `CONSUMER_ADDR` environment
variable, which replaces `consumer_addr` (used by `docker-compose.yml`). The main settings:

- `producers` (p): number of producer threads. Thread i uploads every video in `videos/p<i>/`,
  so create `videos/p1` to `videos/p<p>`.
- `consumers` (c): number of consumer threads.
- `max_queue` (q): queue length; uploads beyond it are dropped.
- `consumer_addr`: where the producer sends uploads (`host:port`).
- `tls_cert`, `tls_key`: where the GUI's certificate lives. It is generated on first start if
  missing.
- `tls_hosts`: extra names/IPs to put in the certificate, e.g. `["192.168.56.10"]` when the GUI
  is opened from another machine by the consumer VM's IP.
- `compress`, `compress_crf`: compressed copies of every upload (bonus 3). Lower CRF means
  better quality and bigger files; 28 is a good demo default. Needs ffmpeg.

The producer waits up to `connect_wait` for the consumer, so start order doesn't matter.

## Preparing the videos

Put a few sample videos in `samples/`, then run (Windows PowerShell):
```powershell
.\setup-videos.ps1               # creates videos/p1 ... videos/p<producers> from config.json
.\setup-videos.ps1 -Count 3      # only 3 samples per folder
```
Each folder gets a copy of every sample, renamed `p<i>_<name>`. The script deletes and recreates `videos/` on every run.

## Run locally

Run the consumer and producer in two separate terminals.

Linux / macOS:
```sh
go build -o bin/p3 ./cmd/p3
bin/p3 consumer    # gRPC on :50051, GUI at https://localhost:8080
bin/p3 producer
```

Windows PowerShell (the binary needs the `.exe` extension):
```powershell
go build -o bin/p3.exe ./cmd/p3
.\bin\p3.exe consumer    # gRPC on :50051, GUI at https://localhost:8080
.\bin\p3.exe producer
```

Use `-config <path>` to load a different config file.

**The GUI is served over HTTPS** with a certificate the consumer generates and signs itself, so
the browser shows a warning the first time ("not secure" / "advanced → proceed"). Accept it once
and the address bar shows HTTPS from then on. Delete `certs/` to force a new certificate.

**ffmpeg** is what makes the 10-second previews and the compressed copies. The Docker image has
it; if you run natively without it, the consumer still works but the GUI previews by playing the
first 10 seconds of the original file and nothing is compressed.

## Run on two VMs

On both VMs, clone the repo and build the image:
```sh
docker build -t p3 .
```

Consumer VM (open TCP 50051 and 8080 in its firewall). Add the VM's own IP to `tls_hosts` in
`config.json` first, so the certificate covers the address you type in the browser:

Linux / macOS:
```sh
docker run -p 50051:50051 -p 8080:8080 \
  -v $PWD/config.json:/app/config.json:ro -v $PWD/data:/app/data -v $PWD/certs:/app/certs p3 consumer
```

Windows PowerShell:
```powershell
docker run -p 50051:50051 -p 8080:8080 -v ${PWD}/config.json:/app/config.json:ro -v ${PWD}/data:/app/data -v ${PWD}/certs:/app/certs p3 consumer
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

`config.json` is mounted into the container, so edits take effect on the next run without
rebuilding.

## Run both on one machine

`docker-compose.yml` starts the consumer and producer as two separate containers:

```sh
docker compose up --build    # build and start both containers
docker compose down          # stop and remove them
```

- **consumer**: gRPC on `50051`, GUI at https://localhost:8080; uploads are saved to `./data`.
- **producer**: reads `./videos` (read-only).
- IP addresses are declared in `docker-compose.yml`, not auto-assigned: consumer `172.28.0.10`,
  producer `172.28.0.11` on the `p3net` subnet (`172.28.0.0/16`). The producer gets
  `CONSUMER_ADDR=172.28.0.10:50051`, which overrides `consumer_addr` in `config.json`.
- The producer is a batch job, so its container runs once and exits; `docker compose logs
  producer` shows what it uploaded. Re-run it on its own with `docker compose up producer`.
- Uploads already in `data/uploads` are hashed at start-up, so re-running the producer over the
  same videos reports them all as duplicates. Empty `data/` first if you want a clean run.

Both containers read `./config.json`. After editing it, run `docker compose up` again.

## How it works

**Queueing (leaky bucket).** The queue is a buffered channel of capacity `q`. A video is offered
to it with a non-blocking send, so when the bucket is full the upload is dropped rather than
waiting. The check happens twice: once when the first chunk arrives (so a video is not received
in full only to be thrown away) and once after the whole file is in. Either way the producer gets
an `UploadStatus` saying the queue is full — that is bonus 1, the consumer telling the producer.

**Producer side.** `p` goroutines, one per input folder, each streaming its videos chunk by
chunk (64 KB) over a single shared gRPC connection. Uploads are therefore simultaneous.

**Consumer side.** gRPC serves every upload stream on its own goroutine; chunks are written
straight to a temp file in the upload folder, so a full queue costs `q` file handles instead of
`q` whole videos in memory. `c` worker goroutines take jobs off the queue, rename the temp file
into place under a unique name (`clip.mp4`, `clip_1.mp4`, …) and then run ffmpeg for the preview
and the compressed copy.

**Synchronisation.** The channel itself is the queue and the drop decision (`select` with
`default`). `sync.Mutex` guards unique filename selection and the duplicate-hash table.
`atomic.Int64` counters feed the GUI without locking. A temp-file-then-rename means the GUI never
lists a half-written video.

**Duplicate detection** Every upload is SHA-256'd as it streams in. The hash is
reserved in a table before the video is queued, so two producer threads uploading the same file
at the same time still produce one saved copy; the loser is told `duplicate of <name>`. On start
the consumer hashes what is already in the upload folder, so restarting it does not forget. A
dropped video releases its reservation and can be sent again.

**Compression** Every upload also gets an H.264/AAC copy at `compress_crf`, which is
what the GUI plays on click. The original is kept untouched. If the copy comes out no smaller and
the original already plays in browsers, the copy is discarded. The log and the GUI stats line
show how many bytes compression saved.