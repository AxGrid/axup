# Getting started

This walkthrough takes you from a fresh clone of the repository to a Redis
container running on your server, in about five minutes. You will need:

- macOS or Linux with Go 1.23+ installed
- A Linux server (Ubuntu 22.04+ recommended) you can SSH into as root (or as a
  user with sudo access — see [auth modes](#auth-modes) below)

## 1. Build the CLI

`make` cross-compiles the remote agent for `linux/amd64` and `linux/arm64`,
embeds both binaries into the CLI, and writes the final binary to `bin/deploy`:

```
cd /path/to/this/repo
make
```

You can put `bin/deploy` somewhere on your `$PATH` if you want:

```
sudo install bin/deploy /usr/local/bin/deploy
deploy version
```

The rest of this guide uses `deploy` as if it were on `$PATH`. Substitute
`./bin/deploy` if you skipped the install step.

## 2. Scaffold a project

Pick a directory for your project and run `deploy init`. It writes a stub
`rulebook.yaml` that you'll fill in next.

```
mkdir ~/projects/myapi && cd ~/projects/myapi
deploy init
cat rulebook.yaml
```

The stub looks like:

```yaml
name: my-app

vars:
  app_name: my-app

bootstrap:
  - name: ping
    command: "uname -a"

deploy:
  - name: hello
    command: "echo deploying my-app"
```

Two top-level sections matter: **`bootstrap:`** is for putting a server into a
working baseline state (install Docker, install supervisor, open firewall
ports, etc.) and **`deploy:`** is for the everyday "build my image, render my
configs, restart my containers" workflow.

## 3. A real first rulebook

Replace the stub with something that actually does work — a baseline Linux
server with Docker installed plus a Redis container running under Docker
Compose. Save this as `rulebook.yaml`:

```yaml
name: myapi

vars:
  app_name: myapi
  redis_port: 6379

bootstrap:
  - name: install prereqs
    apt:
      name: [curl, ca-certificates, gnupg]
      update_cache: true

  - name: install docker
    docker_install: {}

  - name: ensure docker is running on every boot
    service:
      name: docker
      state: started
      enabled: true

deploy:
  - name: app directory
    command: "mkdir -p /opt/{{ .app_name }}"

  - name: render redis compose
    template:
      src: templates/redis.compose.yml.tmpl
      dst: "/opt/{{ .app_name }}/docker-compose.yml"

  - name: bring up redis
    docker_compose:
      dir: "/opt/{{ .app_name }}"
      state: up
      pull: true
```

And the template at `templates/redis.compose.yml.tmpl`:

```yaml
services:
  redis:
    image: redis:7-alpine
    container_name: {{ .app_name }}-redis
    restart: unless-stopped
    ports:
      - "127.0.0.1:{{ .redis_port }}:6379"
    volumes:
      - redis-data:/data

volumes:
  redis-data:
```

The `{{ .app_name }}` and `{{ .redis_port }}` placeholders come from the
top-level `vars:` block. Templates use Go's `text/template` syntax plus all of
the [sprig](https://masterminds.github.io/sprig/) helper functions.

## 4. Bootstrap the server

```
deploy bootstrap --host root@your-server.example.com
```

You should see output like:

```
[root@your-server.example.com] arch=amd64
[root@your-server.example.com] agent uploaded (2 MB → /tmp/deployd-…)
[root@your-server.example.com] ▶ install prereqs (bootstrap.1)
[root@your-server.example.com]   ✓ status=changed
[root@your-server.example.com] ▶ install docker (bootstrap.2)
[root@your-server.example.com]   · status=skipped
[root@your-server.example.com] ▶ ensure docker is running on every boot (bootstrap.3)
[root@your-server.example.com]   · status=skipped
[root@your-server.example.com] done
[root@your-server.example.com] summary: changed=1 skipped=2
```

`status=changed` means the task did work; `status=skipped` means the system
was already in the desired state. Run `deploy bootstrap …` again — you should
see every task skipped this time, because the server is already converged.

## 5. Deploy the project

```
deploy deploy --host root@your-server.example.com
```

After the first run there's a `/opt/myapi/docker-compose.yml` on the server
and the `myapi-redis` container is running:

```
ssh root@your-server.example.com 'docker ps --filter name=myapi-redis'
```

## 6. Check status

`deploy status` is a read-only mode: it reads the agent's state file on the
server and reports whether every file the CLI has ever written is still there
with the right content.

```
deploy status --host root@your-server.example.com
```

You should see:

```
[root@your-server.example.com] arch=amd64
[root@your-server.example.com]   · rulebook=myapi updated_at=… files=1
[root@your-server.example.com] ▶ /opt/myapi/docker-compose.yml (status.1)
[root@your-server.example.com]   ✓ status=in_sync path=/opt/myapi/docker-compose.yml
[root@your-server.example.com] done
[root@your-server.example.com] summary: in_sync=1
```

Try drifting it: edit the file out-of-band on the server, then re-run
`deploy status` — you'll see `status=drift` instead of `in_sync`. Run
`deploy deploy …` again and the file is restored.

## 7. Preview changes without applying them

Add `--check` (alias `--dry-run`) to any `bootstrap` or `deploy` invocation:

```
deploy deploy --check --host root@your-server.example.com
```

Each task that would have changed something reports `status=would_change`;
nothing is actually applied and the agent's state file is untouched.

## Auth modes

If you connect as a non-root user with sudo, replace `root@…` with your
username and add the sudo flags:

```
# NOPASSWD sudo
deploy bootstrap --host deploybot@server --sudo

# sudo with a password
deploy bootstrap --host deploybot@server --ask-sudo-password
```

If you want to use a specific SSH key that isn't picked up by your
ssh-agent:

```
deploy bootstrap --key ~/.ssh/deploy_rsa --host deploybot@server
```

Full auth surface lives in [cli-reference.md](cli-reference.md#auth-flags).

## Where to go next

- Add more task types (apt packages, supervisor processes, copying files):
  [rulebook-reference.md](rulebook-reference.md)
- Manage multiple servers from one rulebook with `inventory.yaml`:
  [inventory-multi-host.md](inventory-multi-host.md)
- Build and push your own Docker images from the same rulebook:
  the [examples/build](../examples/build) example walks through it end-to-end
- Encrypt registry passwords / other secrets:
  [secrets.md](secrets.md)
- Pull reusable rulebook modules (mysql, redis, supervisor templates) from
  another repository: [external-rulebooks.md](external-rulebooks.md)
