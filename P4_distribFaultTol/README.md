# P4 – Distributed Fault-Tolerant Enrollment System

Five application nodes and two database servers, each a separate process/container/VM. Every
node-to-node call is gRPC.

| Node       | Port  | Features                                                     |
|------------|-------|--------------------------------------------------------------|
| `view`     | 8080  | MVC views + controllers (HTTPS), holds no data               |
| `auth`     | 50051 | login / logout, JWT issuing, session log                     |
| `course`   | 50052 | list courses, enroll in open courses, drop                   |
| `grade`    | 50053 | students view grades, faculty upload grades                  |
| `announce` | 50054 | registrar announcements (full CRUD), the extra feature on its own node |
| `pg1`, `pg2` | 5432 | PostgreSQL primary + streaming replica, failover by repmgr  |

```
cmd/p4/              entry point: p4 <view|auth|course|grade|announce>
internal/view/       controllers + html templates (embedded)
internal/auth/       auth node
internal/course/     course node
internal/grade/      grade node
internal/announce/   announce node (registrar announcements)
internal/authz/      JWT signing/verification, gRPC interceptor, revocation checks (shared)
internal/db/         database connection pool + client-side failover (shared)
internal/pool/       gRPC connection pool, view -> nodes (shared)
internal/tlsgen/     self-signed certificate for the HTTPS view
proto/school.proto   gRPC services + generated code
db-init/             creates each node's database and user on first start
```

Demo accounts (password `password`): `alice` (student, 1st year), `bob` (student, 3rd year);
`prof1`, `prof2` (faculty); `registrar` (posts announcements). Each node creates its tables and
seeds them the first time it reaches the database.

## Bonus features

### 1. Redundant database (PostgreSQL primary + replica, repmgr)

- `pg1` starts as the **primary**. `pg2` is a **streaming replica**: every committed write on
  the primary is copied to it continuously.
- **repmgr** runs on both servers. If the primary stops answering (2 failed reconnects, 1s
  apart), repmgr on the replica **promotes it to primary**. When the old primary comes back, it
  **rejoins as a replica** of the new one (pg_rewind, or a fresh clone if that fails). Failover
  is done by an existing tool we configured, as the Q&A allows.
- The application does **client-side failover**. Each node lists every database server
  (`db_hosts`) and only accepts a connection from the one that takes writes
  (`target_session_attrs=read-write`), so after a promotion new connections go to the new
  primary on their own. No restarts, and no proxy that could become a single point of failure.
  The server that last worked is tried first, so a dead server costs a wait only once.
- Each node has **its own database and database user** (`auth`, `course`, `grade`, `announce`),
  and nobody else may connect to it. The nodes share the replicated servers, not their data.

Measured here: after `docker compose stop pg1`, writes fail for about 10 seconds, then work
again on `pg2`. Reads and writes on every other feature resume the same way. While it happens,
pages say the node "can't reach its database" rather than calling the node down.

Replication is asynchronous, so a write committed in the last instant before the primary
dies may not have reached the replica. Synchronous replication would prevent that, but then
every write stops whenever the replica is down, which is the opposite of what we want here.

### 2. Registrar announcements on their own node

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

### 3. Database connection pooling

Each node keeps **one `database/sql` pool** to its database instead of connecting per request:

| Setting | Default | Meaning |
|---|---|---|
| `db_max_open_conns` | 10 | at most this many connections at once; extra requests wait |
| `db_max_idle_conns` | 5 | kept open while idle, ready for the next request |
| `db_conn_max_lifetime` | 30m | a connection is replaced after this long |
| `db_conn_max_idle_time` | 5m | an unused connection is closed after this long |

The `/status` page shows every node's live pool stats: open, in use, idle, how often a request
had to wait for a connection, and how many connections were closed by each limit. It also shows
which server each pool is using and every database server's current role.

(`pool_size` is a different pool: the gRPC connections the view keeps to each node.)

## Run with docker compose (one machine)

```sh
docker compose up --build
# open https://localhost:8080 and accept the self-signed certificate once
```

Fault-tolerance demo:

```sh
docker compose stop grade     # grades page fails; courses, login, announcements still work
docker compose start grade

docker compose stop pg1       # primary database down: ~10s later pg2 is primary, all features work
docker compose start pg1      # pg1 rejoins as the replica (see /status)
```

Reset all data (the databases live in Docker volumes): `docker compose down -v`.

## Run locally (nodes outside Docker)

The nodes still need the database servers, which compose publishes on ports 5441 and 5442
(where `config.json`'s `db_hosts` point):

```sh
docker compose up -d pg1 pg2
go build -o bin/p4 ./cmd/p4
bin/p4 auth & bin/p4 course & bin/p4 grade & bin/p4 announce & bin/p4 view
# open https://localhost:8080
```

Windows PowerShell:
```powershell
docker compose up -d pg1 pg2
go build -o bin/p4.exe ./cmd/p4; Start-Process .\bin\p4.exe auth; Start-Process .\bin\p4.exe course; Start-Process .\bin\p4.exe grade; Start-Process .\bin\p4.exe announce; .\bin\p4.exe view
```

**The view is served over HTTPS** with a certificate the node generates and signs itself, so the
browser warns the first time ("not secure" / "advanced → proceed"). Accept it once and the
address bar shows HTTPS from then on. Delete `certs/` to force a new certificate.

## Run on separate VMs

On every VM: clone the repo, `docker build -t p4 .`, and use the **same `jwt_secret`** in
`config.json`. Set `auth_addr`, `course_addr`, `grade_addr`, `announce_addr` and `db_hosts` to the
other VMs' IPs, and add the view VM's own IP to `tls_hosts` so the certificate covers the
address you type in the browser.

Database VMs (here `192.168.56.21` starts as primary, `192.168.56.22` as replica; run the same
command on each, with that VM's own id and IP):

```sh
docker run -d -p 5432:5432 -v $PWD/db-init:/docker-entrypoint-initdb.d:ro -v pgdata:/bitnami/postgresql \
  -e POSTGRESQL_PASSWORD=p4-admin-password -e REPMGR_PASSWORD=p4-repmgr-password \
  -e REPMGR_PRIMARY_HOST=192.168.56.21 -e REPMGR_PARTNER_NODES=192.168.56.21,192.168.56.22 \
  -e REPMGR_USE_PGREWIND=yes -e REPMGR_CONNECT_TIMEOUT=1 -e REPMGR_RECONNECT_ATTEMPTS=2 -e REPMGR_RECONNECT_INTERVAL=1 \
  -e REPMGR_NODE_ID=1 -e REPMGR_NODE_NAME=pg-1 -e REPMGR_NODE_NETWORK_NAME=192.168.56.21 \
  bitnamilegacy/postgresql-repmgr:17
```

Application VMs:

```sh
docker run -p 50051:50051 -v $PWD/config.json:/app/config.json:ro p4 auth
docker run -p 50052:50052 -v $PWD/config.json:/app/config.json:ro p4 course
docker run -p 50053:50053 -v $PWD/config.json:/app/config.json:ro p4 grade
docker run -p 50054:50054 -v $PWD/config.json:/app/config.json:ro p4 announce
docker run -p 8080:8080   -v $PWD/config.json:/app/config.json:ro -v $PWD/certs:/app/certs p4 view
```

## Configuration

`config.json`. The environment variables `P4_AUTH_ADDR`, `P4_COURSE_ADDR`, `P4_GRADE_ADDR`,
`P4_ANNOUNCE_ADDR` and `P4_DB_HOSTS` (comma-separated) override the addresses; `docker-compose.yml`
uses them.

- `view_listen`, `auth_addr`, `course_addr`, `grade_addr`, `announce_addr`: where each node lives.
- `jwt_secret`: **must be the same on every node.** Change it from the default for anything real.
- `token_ttl`: how long a session lasts (`1h`).
- `rpc_timeout`: how long the view waits for a node before calling it down (`2s`).
- `db_hosts`: every database server. `db_password`: the password of every node's database user;
  must match `db-init/01-p4-databases.sql`.
- `db_max_open_conns`, `db_max_idle_conns`, `db_conn_max_lifetime`, `db_conn_max_idle_time`: the
  database connection pool (see above).
- `db_connect_timeout`: per server, when opening a connection (`500ms`). A new connection may try
  every server, so this times the number of servers must stay under `rpc_timeout`, or a node
  whose database is down would look down itself. `config.Load` refuses to start otherwise.
- `db_probe_timeout`: how long the status page waits for each database server (`500ms`).
- `tls_cert`, `tls_key`, `tls_hosts`: the view's certificate, generated on first start if
  missing. `tls_hosts` adds extra names/IPs, e.g. `["192.168.56.10"]`.
- `pool_size`: gRPC connections the view keeps per node.
- `timezone`: used to read and show announcement dates (`Asia/Manila`).
- `revocation_cache`: how long the course, grade and announce nodes reuse their last "this
  session is live" answer before asking auth again. `0s` (the default) means ask every time, so
  a logout is honoured immediately everywhere.
- `revocation_timeout`: how long they wait for that answer (`500ms`). Must be shorter than
  `rpc_timeout`.

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
