# Gemini web CLI

This project provides Go and Python CLIs that drive Gemini's web UI through an already-running Chrome instance. Neither version launches Chrome or downloads a browser.

## Start Chrome with remote debugging

Start the Chrome instance you want to use with its DevTools endpoint exposed on port 9222. For example:

```bash
google-chrome --headless=new --remote-debugging-port=9222 --user-data-dir=/tmp/gemini-cli-profile
```

`--user-data-dir` must point to a non-default profile directory when Chrome is already open.

## Run headless-shell with Docker Compose

Instead of a local Chrome, run [`chromedp/headless-shell`](https://hub.docker.com/r/chromedp/headless-shell) as a container. It exposes the same DevTools endpoint on port 9222, so both CLIs talk to it unchanged.

Save the following as `docker-compose.yml`:

```yaml
name: headless-shell

services:
  headless-shell:
    image: chromedp/headless-shell
    container_name: headless-shell
    hostname: headless-shell
    restart: always
    init: true

    network_mode: host

    shm_size: 2G

    labels:
      - com.centurylinklabs.watchtower.enable=true
```

Start it, then point the CLI at it:

```bash
docker compose up -d
curl -s http://localhost:9222/json/version
export CHROME_CDP_URL=http://localhost:9222
```

Notes:

- `network_mode: host` publishes 9222 on the host directly, so no `ports:` mapping is needed. On Docker Desktop (macOS/Windows) host networking is not equivalent — drop `network_mode: host` and use `ports: ["9222:9222"]` instead.
- Port 9222 has no authentication. With host networking it is reachable from anything that can reach the host, and anyone who reaches it controls the browser. Keep the host firewalled, or bind it to loopback only.
- `shm_size: 2G` avoids Chrome crashes from the default 64MB `/dev/shm`.
- `init: true` reaps zombie renderer processes.
- The Watchtower label opts the container into automatic image updates, if Watchtower runs on that host.

Useful commands:

```bash
docker compose logs -f headless-shell
docker compose restart headless-shell
docker compose down
```

## Go: build and run

```bash
go build -buildvcs=false -o gemini-web-cli .
./gemini-web-cli
```

Pass another endpoint with `--cdp-url`, or set `CHROME_CDP_URL`.

If a prompt appears to stall, enable diagnostics (written to stderr; prompts and response text are not logged):

```bash
./gemini-web-cli --debug
```

For ACP mode, place the flag before `acp`: `./gemini-web-cli --debug acp`. You can also set `GEMINI_WEB_CLI_DEBUG=1`.

## Python: install and run

```bash
uv venv .venv
uv pip install --python .venv/bin/python -r requirements.txt
.venv/bin/python gemini_web_cli.py
```

The Python entrypoint has the same `repl` (default) and `acp` modes as the Go binary.

Inside the REPL, `/new` opens a fresh Gemini chat and `/exit` quits. If Gemini redirects to sign-in or presents an explicit sign-in gate, the program stops rather than trying to log in.

Gemini's public, anonymous availability and its web-page markup can change. This CLI deliberately does not bypass access controls; it only works while Gemini makes the chat available to the connected browser session.

## Use either version as a Buzz ACP agent

The `acp` mode is an [Agent Client Protocol](https://agentclientprotocol.com/) v1 agent over stdio. It is intended to be launched by Buzz's `buzz-acp` harness. Each ACP session gets its own Gemini tab, while the connected Chrome process remains running after the session ends.

Go:

```bash
export CHROME_CDP_URL=http://localhost:9222
go build -buildvcs=false -o gemini-web-cli .
export BUZZ_ACP_AGENT_COMMAND="$(pwd)/gemini-web-cli"
export BUZZ_ACP_AGENT_ARGS="acp"
buzz-acp
```

Python:

```bash
export CHROME_CDP_URL=http://localhost:9222
export BUZZ_ACP_AGENT_COMMAND="$(pwd)/.venv/bin/python"
export BUZZ_ACP_AGENT_ARGS="$(pwd)/gemini_web_cli.py,acp"
buzz-acp
```

`buzz-acp` supplies the working directory and MCP-server declarations during `session/new`; this browser-backed agent ignores those declarations because its only model interface is the Gemini web chat. It supports `initialize`, `session/new`, `session/prompt`, `session/cancel`, and `session/close`, and returns Gemini output as `session/update` text messages.

See Buzz's [ACP harness guide](https://github.com/block/buzz/blob/main/crates/buzz-acp/README.md) for relay identity, channel membership, and author-gate setup.
