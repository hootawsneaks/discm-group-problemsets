# P4 – Distributed Fault-Tolerant Enrollment System

Five nodes, each a separate process/container/VM. Every node-to-node call is gRPC.

| Node     | Port  | Features                                   |
|----------|-------|--------------------------------------------|
| `view`   | 8080  | MVC views + controllers (HTTPS), no data   |
| `auth`   | 50051 | login / logout, JWT issuing, session log   |
| `course` | 50052 | list courses, enroll in open courses, drop |
| `grade`  | 50053 | students view grades, faculty upload grades|
| `announce` | 50054 | registrar announcements (full CRUD), the extra feature on its own node |

```
cmd/p4/              entry point: p4 <view|auth|course|grade|announce>
internal/view/       controllers + html templates (embedded)
internal/auth/       auth node
internal/course/     course node
internal/grade/      grade node
internal/announce/   announce node (registrar announcements)
internal/authz/      JWT signing/verification, gRPC interceptor, revocation checks (shared)
internal/pool/       gRPC connection pool (shared)
internal/store/      per-node JSON file storage
internal/tlsgen/     self-signed certificate for the HTTPS view
proto/school.proto   gRPC services + generated code
scripts/             automated fault-tolerance walkthrough
```

Demo accounts (password `password`): `alice` (student, 1st year), `bob` (student, 3rd year);
`prof1`, `prof2` (faculty); `registrar` (posts announcements).

## Registrar announcements (announce node)

The registrar posts enrollment schedules per year level, enrollment confirmation reminders,
payment deadlines and general notices. Each announcement has a category, title, body, an
audience (everyone or chosen year levels), an optional start/end date, and a pinned flag.

- **Create / edit / delete:** `registrar` accounts only, enforced on the announce node itself.
- **Read:** everyone. Students only see announcements for everyone or for their own year level.
  Asking for another year's announcement by id returns "not found".
- Year level travels inside the JWT, so the announce node never calls another node to decide
  what a student may see. With auth down, logged-in users can still read and post.
- Announcements are informational. They do not block enrollment, because that would make the
  course node depend on the announce node.
Data is seeded on first start; delete `data/` to reset.

## Run locally

```sh
go build -o bin/p4 ./cmd/p4
bin/p4 auth & bin/p4 course & bin/p4 grade & bin/p4 announce & bin/p4 view
# open https://localhost:8080
```

Windows PowerShell:
```powershell
go build -o bin/p4.exe ./cmd/p4; Start-Process .\bin\p4.exe auth; Start-Process .\bin\p4.exe course; Start-Process .\bin\p4.exe grade; Start-Process .\bin\p4.exe announce; .\bin\p4.exe view
```

**The view is served over HTTPS** with a certificate the node generates and signs itself, so the
browser warns the first time ("not secure" / "advanced → proceed"). Accept it once and the
address bar shows HTTPS from then on. Delete `certs/` to force a new certificate.

## Run with docker compose (one machine)

```sh
docker compose up --build
docker compose stop grade     # grades page fails, courses/login still work
docker compose start grade
```

## Run on separate VMs

On every VM: clone the repo, `docker build -t p4 .`, and use the **same `jwt_secret`** in
`config.json`. On the view VM, set `auth_addr`, `course_addr`, `grade_addr`, `announce_addr` to the other VMs'
IPs, and add the view VM's own IP to `tls_hosts` so the certificate covers the address you type
in the browser. Then on each VM:

```sh
docker run -p 50051:50051 -v $PWD/config.json:/app/config.json:ro -v $PWD/data:/app/data p4 auth
docker run -p 50052:50052 -v $PWD/config.json:/app/config.json:ro -v $PWD/data:/app/data p4 course
docker run -p 50053:50053 -v $PWD/config.json:/app/config.json:ro -v $PWD/data:/app/data p4 grade
docker run -p 50054:50054 -v $PWD/config.json:/app/config.json:ro -v $PWD/data:/app/data p4 announce
docker run -p 8080:8080   -v $PWD/config.json:/app/config.json:ro -v $PWD/certs:/app/certs p4 view
```

## Configuration

`config.json`, with `P4_AUTH_ADDR`, `P4_COURSE_ADDR`, `P4_GRADE_ADDR` and `P4_ANNOUNCE_ADDR`
overriding the node addresses (used by `docker-compose.yml`):

- `view_listen`, `auth_addr`, `course_addr`, `grade_addr`, `announce_addr`: where each node lives.
- `jwt_secret`: **must be the same on every node.** Change it from the default for anything real.
- `token_ttl`: how long a session lasts (`1h`).
- `rpc_timeout`: how long the view waits for a node before calling it down (`2s`).
- `tls_cert`, `tls_key`, `tls_hosts`: the view's certificate, generated on first start if
  missing. `tls_hosts` adds extra names/IPs, e.g. `["192.168.56.10"]`.
- `pool_size`: gRPC connections the view keeps per node (node-to-node pooling, not the
  database connection pool).
- `timezone`: used to read and show announcement dates (`Asia/Manila`).
- `revocation_cache`: how long the course, grade and announce nodes reuse their last "this session is
  live" answer before asking auth again. `0s` (the default) means ask every time, so a logout is
  honoured immediately everywhere.
- `revocation_timeout`: how long they wait for that answer (`500ms`). Must be shorter than
  `rpc_timeout`.
- `data_dir`: where that node keeps its own `<role>.json`.

## Regenerating gRPC code

With `protoc`:
```sh
protoc --go_out=. --go_opt=paths=source_relative \
       --go-grpc_out=. --go-grpc_opt=paths=source_relative proto/school.proto
```

Or without installing `protoc`, using the `buf.yaml` / `buf.gen.yaml` in this folder:
```sh
go install github.com/bufbuild/buf/cmd/buf@latest
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
buf generate
```
