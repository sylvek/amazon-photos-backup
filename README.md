# amazon-photos-backup

Continuously backs up your whole Amazon Photos library (photos and videos,
full-resolution originals) to a local disk.

You authenticate **once**, by handing over the cookies of your browser
session. The service then runs in the background in Docker, keeps the session
on disk, and picks up by itself after a reboot.

> **Good to know.** Amazon Photos has no public API. This program uses the
> website's internal, undocumented API: it can change without notice and break
> the tool. Amazon can also revoke a session at any time (password change,
> security check). When that happens the service tells you, and the "login"
> step has to be done again: no tool can guarantee a session that lasts
> forever.

## Getting started

Requirement: Docker with the Compose plugin.

### 1. Prepare the data directory

```bash
mkdir -p data
```

```bash
printf 'PUID=%s\nPGID=%s\n' "$(id -u)" "$(id -g)" > .env
```

The container runs as your own user, so the photos belong to you on the host.
Do create `data` yourself: otherwise Docker creates it as `root` and the
service cannot write to it.

### 2. Copy the cookies from your browser

1. Open a **private browsing window** and sign in at
   <https://www.amazon.fr/photos> (or your country's Amazon site) with
   "Keep me signed in" checked.
2. Open the developer tools (F12), go to the **Network** tab, and reload the
   page.
3. Click a request to `www.amazon.fr` (for instance one whose URL contains
   `drive/v1`), then under **Request Headers** copy the full value of the
   `Cookie` header.
4. Paste it into a file named `cookies.txt`.
5. Close the private window **without signing out**: signing out would
   invalidate the session you just copied.

`document.cookie` in the console is not enough: the authentication cookie
(`at-acbfr`) is `HttpOnly` and does not show up there.

### 3. Get the image and log in

The image is published by CI to `ghcr.io/sylvek/amazon-photos-backup`
(amd64 and arm64):

```bash
docker compose pull
```

Or, to build it yourself from the sources:

```bash
docker compose build
```

```bash
docker compose run --rm -T amazon-photos-backup login < cookies.txt
```

```bash
rm cookies.txt
```

The `login` command checks the cookies with Amazon before saving them. If
they are rejected, nothing is written.

### 4. Start the service

```bash
docker compose up -d
```

The first backup starts right away. The following ones run every 6 hours.
Thanks to `restart: unless-stopped`, the service comes back with the machine,
without a new login.

## Monitoring

```bash
docker compose logs -f
```

```bash
docker compose exec amazon-photos-backup amazon-photos-backup status
```

`docker ps` also shows the container's health. It turns `unhealthy` when the
session has to be renewed, or when no backup has succeeded for three
intervals.

| State           | Meaning                                                           |
|-----------------|-------------------------------------------------------------------|
| `syncing`       | a backup is running (the first one can take a long time)          |
| `ok`            | everything is backed up                                           |
| `degraded`      | the backup finished, but some items failed (see `status`)         |
| `error`         | the last backup could not complete; it is retried automatically   |
| `auth_required` | Amazon rejects the session: the login step must be done again     |

## When the session expires

The service logs the error, switches to `auth_required`, sends a notification
if `NOTIFY_URL` is set, and waits. Repeat steps 2 and 3 (`cookies.txt`, then
`login`): the running service notices the new cookies within 30 seconds and
resumes where it stopped. There is no need to restart it.

## Configuration

Environment variables, set in `docker-compose.yml`:

| Variable               | Default        | Purpose                                                              |
|------------------------|----------------|----------------------------------------------------------------------|
| `SYNC_INTERVAL`        | `6h`           | pause between two backups (minimum `5m`)                             |
| `DOWNLOAD_CONCURRENCY` | `4`            | simultaneous downloads (1 to 16)                                     |
| `NOTIFY_URL`           | —              | alert webhook: plain-text `POST`, compatible with [ntfy](https://ntfy.sh) |
| `AMAZON_TLD`           | from cookies   | Amazon site: `fr`, `de`, `co.uk`, `com`…                             |
| `USER_AGENT`           | Chrome/Linux   | User-Agent presented to Amazon (ideally your browser's)              |
| `TZ`                   | `Europe/Paris` | time zone of the logs                                                |
| `LOG_LEVEL`            | `info`         | `debug`, `info`, `warn`, `error`                                     |

## What the service does

- **Layout**: `data/photos/YYYY/MM/<original name>`, from the date the photo
  was taken (failing that, the upload date; otherwise `unknown/`). The file's
  modification time is set to the date it was taken.
- **Incremental**: a file that is already backed up is never downloaded again.
- **Verified**: every download is checked (size and MD5 announced by Amazon)
  before it gets its final name. An interrupted download leaves no incomplete
  file under a photo's name (after a power cut, a harmless hidden `.dl-…`
  file may remain and can be deleted). If Amazon serves the same bytes twice
  in a row although they do not match the announced MD5, the file is kept and
  flagged (`mismatched`) rather than thrown away.
- **Careful when things break**: a full disk or a read-only volume stops the
  backup with an error, instead of counting every file as "failed". If 20
  downloads fail in a row, the run stops and the state becomes `error`.
- **Never destructive**: nothing is ever deleted or overwritten locally. A
  photo deleted on Amazon stays in the backup. If two photos share a name, or
  if a photo is modified on Amazon, the new one is stored next to the other
  with a suffix (`IMG_0001_AbCd1234.jpg`).
- **Self-repairing**: a backed-up file that is deleted locally is downloaded
  again. If the index (`data/state/index.json`) is lost, the files already
  present are recognised by their MD5 instead of being downloaded again.

## Security

- `data/state/cookies.json` gives access to your Amazon account. It is created
  with mode `0600`; do not share it and do not commit it.
- Cookies are only ever sent to `amazon.<tld>` and its subdomains, never to
  the content servers Amazon redirects downloads to.
- The container runs unprivileged, with a read-only filesystem outside
  `/data`.

## Known limits

- Amazon's search API returns at most 9999 items per query. Large libraries
  are therefore walked by year, then by month, then by day. More than 9999
  items on a single day cannot all be listed: the service reports them
  (`unreachable`) instead of ignoring them.
- Only your own library is backed up (not other members' family vault).
- The program could only be tested against a fake Amazon server included in
  the tests; see "Development".

## Development

Go 1.24, standard library only.

```bash
go test ./...
```

A fake server is provided to try the service without an Amazon account:

```bash
go run ./internal/amazon/amazontest/fakeserver
```

It prints a value for `AMAZON_BASE_URL` and cookies it accepts.

### Continuous integration

`.github/workflows/ci.yml` runs the formatting check, `go vet` and the tests
on every push and pull request. On `main`, the image is published with the
`latest` tag; a git tag `v1.2.3` additionally publishes `1.2.3` and `1.2`.
Pull requests build the image without publishing it.
