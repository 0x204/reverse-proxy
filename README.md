# reverse-proxy

A small reverse proxy: every request it receives is forwarded to one configured
backend.

## Run

```bash
git clone https://github.com/0x204/reverse-proxy
cd reverse-proxy
sudo snap install go --classic   # or: https://go.dev/dl/
go mod tidy                      # resolves go.sum from the versions in go.mod
cp config.json.example config.json
$EDITOR config.json              # set "backend"
go run .
```

`config.json` is gitignored, because it holds the address of an internal host.
If it does not exist, the proxy prompts for a backend on first run and writes
the file itself — but only when stdin is a terminal, so under systemd, Docker
without `-it`, or a prefork worker it fails with a clear message instead of
silently starting with no backend.

## Configuration

| Key | Default | Meaning |
| --- | --- | --- |
| `backend` | *(required)* | Origin to forward to. Must include a scheme, e.g. `http://127.0.0.1:8080`. |
| `listen` | `:8080` | Address to bind. Ports below 1024 need root or `CAP_NET_BIND_SERVICE`. |
| `allow_origins` | `""` | Adds CORS headers to proxied responses: `*` or a comma-separated origin list. Empty forwards the backend's own CORS policy untouched. |
| `prefork` | `false` | One worker process per CPU. Off by default: it re-executes the program, and children start without a stdin. |

An invalid backend (missing scheme, non-numeric port, or one pointing at this
proxy's own listen address) is rejected at startup rather than turning into a
failure on every request.

## What it forwards

- Adds `X-Forwarded-For`, `X-Real-Ip`, `X-Forwarded-Host` and
  `X-Forwarded-Proto`. Any client-supplied `X-Forwarded-For` is overwritten,
  not appended to, since this proxy is the edge.
- Strips hop-by-hop headers in both directions (RFC 9110 §7.6.1).
- Passes paths through byte-for-byte: `%2F` is not decoded and `..` is not
  collapsed before the backend sees it.
- Only accepts origin-form request targets (`/path?query`).
- Returns a plain `502` when the backend is unreachable; the underlying error
  (which names the backend's address) goes to the log only.

## Limitations

- **No WebSocket or upgrade support.** `Upgrade` is a hop-by-hop header and is
  stripped; `fasthttp.Client` cannot hijack the connection for a 101 response.
- **No response compression.** The backend receives the client's
  `Accept-Encoding` and should compress itself. Compressing here would mean
  buffering each response in full, which defeats response streaming.
- **60s backend timeout**, so server-sent events and other endpoints that hold a
  response open indefinitely will be cut off.
