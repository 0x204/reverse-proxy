# reverse-proxy

Forwards every request it receives to one backend. Uses only the Go standard
library.

## Run

```bash
git clone https://github.com/0x204/reverse-proxy
cd reverse-proxy
cp config.json.example config.json   # then set "backend"
go run .
```

If `config.json` is missing and stdin is a terminal, the proxy asks for a
backend and writes the file. Otherwise it exits with an error.

`config.json` is gitignored because it contains the address of an internal host.

## Configuration

| Key | Default | Meaning |
| --- | --- | --- |
| `backend` | required | URL to forward to, e.g. `http://127.0.0.1:8080`. A path in it is prefixed to every request path, and a query is merged into every query. |
| `listen` | `:80` | Address to listen on. Ports below 1024 need root or `CAP_NET_BIND_SERVICE`. |
| `allow_origins` | `""` | `*` or a comma-separated list of origins. When set, the proxy answers CORS preflights and sets `Access-Control-Allow-Origin` on responses, replacing the backend's. When empty, CORS is left to the backend. |

## Behavior

- Requests and responses are streamed, not buffered.
- Paths are forwarded as received: `%2F` stays encoded and `..` is not collapsed.
- `X-Forwarded-For`, `X-Forwarded-Host`, `X-Forwarded-Proto` and `X-Real-Ip`
  are set from the connection. Any values the client sent are dropped.
- Hop-by-hop headers, including those named in `Connection`, are removed in
  both directions.
- Only origin-form request targets (`/path?query`) are accepted.
- Request bodies over 64 MiB get a 413.
- WebSocket and other `Upgrade` requests are passed through.
- If the backend fails before sending a response, the client gets a plain 502
  and the error goes to the log.
- Each instance adds an `X-Proxy-Id` header. A request that comes back to the
  same instance gets a 508 instead of looping.

## Timeouts

Connections to clients and to the backend are closed once they stop making
progress for 60 seconds. Long downloads and event streams work as long as data
keeps flowing. A WebSocket that is silent for 60 seconds is closed.

On SIGINT or SIGTERM the proxy stops accepting connections and gives in-flight
requests 15 seconds to finish. A second signal exits immediately.

## Limitations

- No TLS. The proxy listens on plain HTTP.
- No response compression. The backend gets the client's `Accept-Encoding` and
  should compress itself.
