# P4 – Distributed Fault-Tolerant Enrollment System

Four nodes, each a separate process/container/VM. Every node-to-node call is gRPC.

| Node     | Port  | Features                                   |
|----------|-------|--------------------------------------------|
| `view`   | 8080  | MVC views + controllers (HTML), no data    |
| `auth`   | 50051 | login / logout, JWT issuing, session log   |
| `course` | 50052 | list courses, enroll in open courses       |
| `grade`  | 50053 | students view grades, faculty upload grades|

```
cmd/p4/              entry point: p4 <view|auth|course|grade>
internal/view/       controllers + html templates (embedded)
internal/auth/       auth node
internal/course/     course node
internal/grade/      grade node
internal/authz/      JWT signing/verification + gRPC interceptor (shared)
internal/store/      per-node JSON file storage
proto/school.proto   gRPC services + generated code
```

## How sessions and fault tolerance work

- Logging in calls `Auth.Login`, which returns an HS256 JWT (user, role, expiry). The view stores it
  in an HttpOnly cookie and sends it as `authorization: Bearer ...` metadata on every gRPC call.
- **Every node verifies the JWT itself** using the shared `jwt_secret`, so the session is valid on all
  nodes without asking the auth node. If auth is down, people who are already logged in keep working;
  only login/logout fail.
- The auth node records every issued session (by token ID) in `data/auth.json` and marks it on logout.
- Each node owns its own data file (`data/<node>.json`); no node reads another node's data.
- The view connects lazily and uses a 2s per-call timeout. If a node is unreachable, only the page
  that needs it shows "The X node is down"; the rest of the site keeps working. When the node comes back,
  the view reconnects within a few seconds.

Demo accounts (password `password`): `alice`, `bob` (students); `prof1`, `prof2` (faculty).
Data is seeded on first start; delete `data/` to reset.

## Run locally

```sh
go build -o bin/p4 ./cmd/p4
bin/p4 auth & bin/p4 course & bin/p4 grade & bin/p4 view
# open http://localhost:8080
```

## Run with docker compose (one machine)

```sh
docker compose up --build
docker compose stop grade     # grades page fails, courses/login still work
docker compose start grade
```

## Run on separate VMs

On every VM: clone the repo, `docker build -t p4 .`, and use the **same `jwt_secret`** in `config.json`.
On the view VM, set `auth_addr`, `course_addr`, `grade_addr` to the other VMs' IPs. Then on each VM:

```sh
docker run -p 50051:50051 -v $PWD/config.json:/app/config.json:ro -v $PWD/data:/app/data p4 auth
docker run -p 50052:50052 -v $PWD/config.json:/app/config.json:ro -v $PWD/data:/app/data p4 course
docker run -p 50053:50053 -v $PWD/config.json:/app/config.json:ro -v $PWD/data:/app/data p4 grade
docker run -p 8080:8080   -v $PWD/config.json:/app/config.json:ro p4 view
```

Open those ports in each VM's firewall. Shutting down any one VM disables only that node's features.

## Regenerating gRPC code

```sh
protoc --go_out=. --go_opt=paths=source_relative \
       --go-grpc_out=. --go-grpc_opt=paths=source_relative proto/school.proto
```
