# Thermal

An HTTP server that connects to your ESC/POS printer and offers a feature-rich API for printing all kinds of documents.

It is built on [`github.com/connordoman/escpos`](https://github.com/connordoman/escpos) and has been tested with a Rongta RP326. It is meant to run on a Raspberry Pi with the printer attached over USB, but runs anywhere Go does.

- **Opinionated routes** print Markdown, any Unicode text (emoji, kanji, ...) and JSON documents made of blocks.
- **Simple routes** print raw bytes and plain text, and report on the printer.
- **A durable queue**: requests are rendered, stored in SQLite and printed one at a time in priority order. Every job is kept for auditing, and can be cancelled or retried.
- **API keys** that are easy to recognise and rotate, with scopes.

## Quick start

```sh
go run .
```

On first start (when there is no database) the server prints a bootstrap admin key:

```
  Bootstrap admin API key (shown once):

    thm_4ew1ay37_Y53yAWuKVH3CJbNfELMeHj4GItqQJycq
```

Use it to create a key for each app, then retire it:

```sh
ADMIN=thm_4ew1ay37_...
curl -X POST localhost:8080/v1/keys -H "Authorization: Bearer $ADMIN" \
  -d '{"name":"pos","scopes":["print","read"]}'
# {"secret":"thm_ji4oyx8x_3mNk...", "key":{...}, "warning":"store this key now; ..."}

KEY=thm_ji4oyx8x_3mNk...
curl -X POST localhost:8080/v1/print/text -H "Authorization: Bearer $KEY" --data-binary 'Hello, world'
curl -X POST localhost:8080/v1/print/markdown -H "Authorization: Bearer $KEY" --data-binary @notes.md
curl -X POST localhost:8080/v1/print/json -H "Authorization: Bearer $KEY" --data-binary @examples/receipt.json
```

If you lose every admin key, run `thermal -new-admin-key <name>` on the server to issue a new one.

## Configuration

Settings come from the environment, or a `.env` file in the working directory.

| Variable                                | Default      |                                                                                             |
| --------------------------------------- | ------------ | ------------------------------------------------------------------------------------------- |
| `ESCPOS_CONNECTION`                     | `usb`        | How to reach the printer; see below                                                         |
| `ESCPOS_VENDOR_ID`, `ESCPOS_PRODUCT_ID` |              | Optional hex IDs that narrow USB detection, e.g. `0fe6` and `811e`                          |
| `ESCPOS_USB_SERIAL`                     |              | Optional USB serial number that narrows USB detection                                       |
| `ESCPOS_PAPER_WIDTH`                    | `80mm`       | `80mm` (or `80`), `82mm`, `60mm`, `58mm`, or a printable width in dots such as `576`        |
| `ESCPOS_TIMEOUT`                        | `2s`         | Timeout for each printer query                                                              |
| `THERMAL_ADDR`                          | `:8080`      | Listen address                                                                              |
| `THERMAL_DB`                            | `thermal.db` | SQLite database file                                                                        |
| `THERMAL_MAX_BODY`                      | `16777216`   | Request body limit in bytes                                                                 |
| `THERMAL_RETENTION`                     | `720h`       | How long job bodies are kept before being purged (job records stay); `0` keeps them forever |
| `THERMAL_IMAGE_ALLOW_PRIVATE_HOSTS`     | `false`      | Allow image URLs on loopback/private networks                                               |
| `THERMAL_IMAGE_MAX_BYTES`               | `10485760`   | Largest image that will be downloaded or decoded                                            |
| `THERMAL_IMAGE_TIMEOUT`                 | `10s`        | Time limit for downloading one image                                                        |
| `THERMAL_DEBUG`                         | `false`      | Debug logging and Gin debug mode                                                            |

`ESCPOS_CONNECTION` is an [`escpos.Open`](https://github.com/connordoman/escpos#connection-strings-and-reconnecting) connection string:

| Value | |
|---|---|
| `usb` | Find a USB printer automatically. Linux and Windows use the OS printer driver (no cgo); macOS, and printers not bound to a printer driver, use libusb |
| `usb?vid=0fe6&pid=811e&serial=...` | The same, narrowed down (the `ESCPOS_VENDOR_ID`, `ESCPOS_PRODUCT_ID` and `ESCPOS_USB_SERIAL` settings are merged in) |
| `libusb` | Find a USB printer with libusb only |
| `file:/dev/usb/lp0` | A device node |
| `tcp://192.168.1.50:9100` | Ethernet (the port defaults to 9100) |
| `serial:/dev/ttyUSB0?baud=9600` | RS-232 (`serial:COM3` on Windows); also `databits`, `parity`, `stopbits` |
| `discard` | Accept and record jobs without printing, for testing |

The connection opens on first use and reopens after a failure, so the printer can be unplugged, switched off or replaced while the server runs.

libusb needs cgo. It is built in on macOS, and on other platforms with `-tags libusb`. Linux and Windows builds are otherwise cgo-free (SQLite uses a pure Go driver), so building for a Pi is just:

```sh
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o thermal .
```

## Authentication

Send a key in either header:

```
Authorization: Bearer thm_ji4oyx8x_3mNktPjsRjbGX527EzNrhwkJx4JLAYE0
X-API-Key: thm_ji4oyx8x_3mNktPjsRjbGX527EzNrhwkJx4JLAYE0
```

A key is `thm_` + an 8-character public ID + a 32-character secret. The ID appears in logs, the job list and the audit log, so you can tell which app did what without ever seeing a secret. The server stores only a SHA-256 hash of each secret.

| Scope   | Allows                                                   |
| ------- | -------------------------------------------------------- |
| `print` | Submitting, cancelling and retrying jobs                 |
| `read`  | Printer information, the queue and job records           |
| `admin` | Everything, plus managing keys and reading the audit log |

| Endpoint                              |                                                                                                                                      |
| ------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------ |
| `POST /v1/keys`                       | `{"name", "scopes", "expires_in"?: "720h", "expires_at"?: RFC 3339}`. Returns the secret once                                        |
| `GET /v1/keys[?include_revoked=true]` | List keys                                                                                                                            |
| `GET /v1/keys/:id`                    | One key                                                                                                                              |
| `PATCH /v1/keys/:id`                  | Change `name`, `scopes`, `expires_in` or `expires_at` (`null` clears it)                                                             |
| `POST /v1/keys/:id/rotate`            | New secret, same ID. With `{"grace": "24h"}` the old secret keeps working for that long, so clients can switch over without downtime |
| `DELETE /v1/keys/:id`                 | Revoke                                                                                                                               |
| `GET /v1/whoami`                      | The calling key (any scope)                                                                                                          |

The last usable admin key cannot be revoked or lose its admin scope.

## Printing

Every print endpoint takes the body as-is (no form encoding) and these query parameters:

| Parameter             | Default   |                                                                                     |
| --------------------- | --------- | ----------------------------------------------------------------------------------- |
| `cut`                 | `partial` | `none`, `partial` or `full` (the RP326 always cuts partially)                       |
| `feed`                | `0`       | Extra dots of paper fed before the cut (8 dots = 1 mm)                              |
| `copies`              | `1`       | 1–20                                                                                |
| `priority`            | `0`       | −100 to 100; higher prints first                                                    |
| `label`               |           | Shown in the job list                                                               |
| `open_drawer`, `beep` | `false`   | Kick the cash drawer or beep after printing                                         |
| `wait`                |           | Wait up to this long (e.g. `30s`, max `5m`) for the job to finish before responding |
| `dry_run`             | `false`   | Return the rendered ESC/POS bytes instead of queueing them                          |

A submission returns `202 Accepted` with the job and its queue position, or `200 OK` if `wait` saw it finish (check `job.status`, which may be `failed`). Bad input returns `400` or `422` with an error code, a message and, for documents, the path of each problem.

### `POST /v1/print/text`

Plain text, transliterated to ASCII ("Café — “naïve”" prints as `Cafe - "naive"`) and word-wrapped. Control characters are stripped, so text can never smuggle printer commands into a job. Options: `font=a|b`, `size`, `width`, `height` (1–8), `bold`, `underline=0|1|2`, `invert`, `align=left|center|right`, `wrap=false`.

### `POST /v1/print/raw`

Bytes sent to the printer untouched. Nothing is added unless you ask for `cut`, `feed`, `open_drawer` or `beep`.

### `POST /v1/print/markdown`

GitHub-flavoured Markdown, printed with the printer's own fonts and styles:

| Markdown                              | Printed as                                                                                               |
| ------------------------------------- | -------------------------------------------------------------------------------------------------------- |
| `# H1` / `## H2` / `### H3` / `####`+ | Double size bold / double height bold / bold underlined / bold                                           |
| `**bold**`, `*italic*`                | **Bold**, <span style="text-decoration:underline;">underlined</span> (printers have no italics)          |
| `` `code` ``, code blocks             | White on black, Font B                                                                                   |
| `~~strike~~`                          | `~strike~` (printers cannot strike through)                                                              |
| Lists, task lists, quotes             | Bullets with hanging indents, `[x]`/`[ ]`, `│` bars; all nest                                            |
| Tables                                | Columns sized to fit, cells wrap, alignment honoured                                                     |
| `---`                                 | A full-width rule                                                                                        |
| `![alt](url)`                         | The image, fetched with the same safeguards as image blocks (`images=false` prints the alt text instead) |
| Links                                 | See `links` below                                                                                        |
| Footnotes, `:emoji:` shortcodes       | Supported                                                                                                |

Options:

- `links=inline` (default) prints `text (url)` in the small font. `links=footnotes` numbers links and lists their URLs at the end. `links=qr` does the same but also prints a QR code for each URL. `links=hide` prints only the text.
- `unicode=image` (default) prints any paragraph, heading, list item or table containing characters the printer's fonts lack (emoji, CJK, ...) as an image, using the UTF-8 renderer below. `unicode=transliterate` prints the closest ASCII instead.

### `POST /v1/print/utf8`

Any Unicode text, drawn as an image in [GNU Unifont](https://unifoundry.com/unifont/). It is a bitmap font that covers every assigned code point, including emoji, kanji, hangul, Cyrillic, Greek, symbols and box drawing, and its pixel lettering looks at home on a thermal print head. Lines wrap at spaces, and anywhere between CJK characters following Japanese line-breaking rules.

Options: `scale` (1–8, default 2; 1.5 matches Font A's 12×24 cells), `bold`, `invert`, `align`, `line_gap`, `wrap=false`, and `format=png` to get a PNG preview instead of printing.

Arabic and Hebrew are drawn left to right without joining, and emoji print in monochrome without skin-tone or ZWJ combinations.

### `POST /v1/print/json`

A document of blocks, validated against a JSON Schema served at [`/v1/schema/job.json`](internal/render/schema/job.v1.json). The body is either a document or a bare array of blocks:

```json
[
  { "type": "text", "content": "hello, world" },
  { "type": "qr_code", "content": "https://example.com" },
  { "type": "cut", "content": "PARTIAL" }
]
```

A document adds metadata and job options (query parameters override them):

```json
{
  "$schema": "http://localhost:8080/v1/schema/job.json",
  "version": 1,
  "metadata": { "label": "Order #1042", "order_id": 1042 },
  "options": { "cut": "partial", "copies": 2, "priority": 10, "unicode": "image" },
  "blocks": [ ... ]
}
```

See [`examples/receipt.json`](examples/receipt.json) for a receipt and [`examples/everything.json`](examples/everything.json) for every block type.

| Block                                             |                                                                                             |
| ------------------------------------------------- | ------------------------------------------------------------------------------------------- |
| `text`                                            | A paragraph, with `style`, `align` and `wrap`; or `spans` for mixed styles in one paragraph |
| `heading`                                         | Levels 1–6, styled like Markdown headings                                                   |
| `markdown`                                        | Markdown inside a document                                                                  |
| `unicode` / `utf8`                                | Text drawn with Unifont                                                                     |
| `key_value`                                       | Label left, value right, optional dot leaders: receipt line items and totals                |
| `columns`                                         | Side-by-side text with relative widths                                                      |
| `table`                                           | Headers, rows, per-column alignment, optional borders                                       |
| `list`                                            | Bulleted or numbered                                                                        |
| `box`                                             | Text in a single or double frame                                                            |
| `rule`                                            | A full-width line of any character                                                          |
| `feed`                                            | Lines or dots of paper                                                                      |
| `qr_code`                                         | QR code with size, error correction and an optional caption                                 |
| `barcode`                                         | UPC-A/E, EAN-13/8, Code 39/93/128, ITF, Codabar, with HRI options                           |
| `pdf417`                                          | PDF417                                                                                      |
| `image`                                           | PNG, JPEG, GIF or WebP from a URL, data URI or base64, with width, alignment and dithering  |
| `datetime`                                        | The current time in any format and time zone                                                |
| `counter`                                         | The printer's serial-number counter                                                         |
| `group`                                           | Child blocks sharing a style and alignment; groups nest                                     |
| `page`                                            | Child blocks in page mode, for rotated printing                                             |
| `cut`, `drawer`, `beep`                           | Paper cut, cash drawer kick, buzzer                                                         |
| `code_page`, `charset`, `line_spacing`, `margins` | Printer settings for the blocks that follow                                                 |
| `initialize`, `self_test`, `raw`                  | Printer reset, self-test page, raw bytes in hex or base64                                   |

Styles (`style`, `key_style`, ...) take `bold`, `underline` (`true` or 0–2), `double_strike`, `invert`, `font` (`A`/`B`), `size`, `width`, `height` (1–8) and `upside_down`. Text blocks containing characters the printer lacks are drawn with Unifont unless `options.unicode` is `transliterate`.

### Images

Images are fetched and decoded while the request is handled, so problems are reported straight away. Because a URL makes the server fetch something on the client's behalf, loading is defensive:

- Only `http`, `https` and `data:` sources are accepted. URLs with credentials are refused, and so are redirects to other schemes (3 redirects at most).
- Unless `THERMAL_IMAGE_ALLOW_PRIVATE_HOSTS` is set, connections to loopback, private, link-local (including cloud metadata at 169.254.169.254), carrier-grade NAT and other non-public addresses are refused. The check runs on the resolved address of every connection, so DNS rebinding cannot get around it. Environment proxies are ignored.
- The `Content-Type` must be PNG, JPEG, GIF or WebP (SVG is never accepted). The bytes must sniff as the same type, and the format reported by the image header must agree as well.
- Downloads are limited in time and size. Image dimensions are checked from the header before decoding (40 MP and 12,000 px a side at most), so a small file claiming huge dimensions is rejected before any memory is allocated. A printed image can be at most 1 m tall, and a job can hold at most 16 images.

## Printer and queue

| Endpoint                 |                                                                                                                                                                                                                                                 |
| ------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `GET /v1/printer`        | Connection type and target, USB descriptor (path, IDs, manufacturer, product, serial, IEEE 1284 ID), what the printer reports about itself (firmware, name, serial, cutter), live status and worker state. `?status=false` skips the live query |
| `GET /v1/printer/status` | Live status: ready, cover open, paper end, cutter error and so on                                                                                                                                                                               |
| `GET /v1/queue`          | Worker state, job counts by status, and queued jobs in print order                                                                                                                                                                              |

The worker prints one job at a time. Before each job it checks the printer's real-time status. If the cover is open or the paper has run out it waits (state `waiting`), and if the printer is unreachable it backs off and retries (state `offline`). In both cases jobs stay queued and nothing is lost. When the connection can read responses, the worker appends a process-ID request (GS ( H) to each job and waits for the printer to report it, so `completed` with `"confirmed": true` means the job was actually printed, not just sent.

Jobs interrupted by a shutdown are marked `failed`, not reprinted, since they may have partly printed. Use retry if you want them again. On shutdown the server lets the job in progress finish.

## Auditing

| Endpoint                   |                                                                                                                                   |
| -------------------------- | --------------------------------------------------------------------------------------------------------------------------------- |
| `GET /v1/jobs`             | Newest first. Filters: `status`, `kind`, `key` (key ID), `since` (RFC 3339), `before` (job ID cursor), `limit` (≤ 500)            |
| `GET /v1/jobs/:id`         | One job: who submitted it (key, IP, user agent), sizes, SHA-256 of the payload, attempts, error, timings                          |
| `GET /v1/jobs/:id/source`  | The request body as received                                                                                                      |
| `GET /v1/jobs/:id/payload` | The exact ESC/POS bytes sent to the printer                                                                                       |
| `POST /v1/jobs/:id/cancel` | Cancel a queued job                                                                                                               |
| `POST /v1/jobs/:id/retry`  | Queue a copy of any job (`retry_of` links them)                                                                                   |
| `GET /v1/audit`            | Key creation, rotation, revocation and changes, plus job cancellations and retries. Filters: `action`, `actor`, `before`, `limit` |

Bodies and payloads are purged after `THERMAL_RETENTION`; the job records themselves are kept.

## Running on a Raspberry Pi

Recipes are in the [`justfile`](justfile). Set `PI_HOST` (default `pi@pos.local`) to your Pi's SSH address and `PI_ARCH=arm` if it runs 32-bit Raspberry Pi OS. Everything builds on your Mac and is copied over, so the Pi never compiles anything.

### With Docker

```sh
just docker-deploy-pi   # build the image, load it on the Pi, start it with compose
just logs-pi            # the bootstrap key is printed on first start
```

The image is about 26 MB: a static binary on distroless, running as a non-root user. Go cross-compiles on the build machine rather than under emulation, so a rebuild takes seconds. The database lives in the `thermal-data` volume, and a `.env` next to `compose.yaml` on the Pi is passed to the container.

[`compose.yaml`](compose.yaml) shares `/dev` with the container so the printer can be unplugged and replugged (its `/dev/usb/lpN` node comes and goes). A device cgroup rule allows only USB printers (major number 180) to be opened. If the printer is always connected, you can replace both with `devices: ["/dev/usb/lp0"]`.

### With systemd

```sh
just deploy-pi   # build, copy to /usr/local/bin/thermal, restart the service
```

The Linux `usblp` driver creates `/dev/usb/lp0` for the printer, owned by the `lp` group. Add the service user to that group (`sudo usermod -aG lp thermal`), then install this unit:

```ini
# /etc/systemd/system/thermal.service
[Unit]
Description=Thermal print server
After=network-online.target
Wants=network-online.target

[Service]
User=thermal
Group=lp
WorkingDirectory=/var/lib/thermal
ExecStart=/usr/local/bin/thermal
Restart=on-failure
Environment=THERMAL_ADDR=:8080

[Install]
WantedBy=multi-user.target
```

The bootstrap key is printed to the journal on first start: `journalctl -u thermal | grep -A2 Bootstrap`.

## Development

```sh
just test         # go test ./...
just check        # gofmt, vet and tests
just dev          # run with debug logging and dev.db
just dev-discard  # the same without a printer
just generate     # sqlc generate, after changing queries or migrations
just build-mac    # bin/thermal-darwin-arm64, with libusb
just build-pi     # bin/thermal-linux-arm64, static
```

Until `github.com/connordoman/escpos` is published, `thermal` builds against `../escpos` through the Go workspace, and the Docker recipes pass it to the build with `--build-context escpos=../escpos`. Once escpos is tagged and required in `go.mod`, a plain `docker build .` fetches it from the Go proxy instead.

| Path                   |                                                                                           |
| ---------------------- | ----------------------------------------------------------------------------------------- |
| `internal/server`      | Gin routes and handlers                                                                   |
| `internal/render`      | Text, Markdown, Unicode and block-document renderers; image loading; the JSON Schema (layout and Unifont come from `escpos/layout` and `escpos/unifont`) |
| `internal/queue`       | The job queue and print worker                                                            |
| `internal/device`      | Printer connection management                                                             |
| `internal/auth`        | API keys                                                                                  |
| `internal/store`       | SQLite, migrations (`migrations/`), sqlc queries (`queries/`) and generated code (`dbq/`) |
