# Amazon Photos Backup — design

## Intent

Back up a whole Amazon Photos library (amazon.fr) to a local disk,
continuously and unattended: authenticate once, then the service runs in the
background and survives reboots.

## Goals

- Download the originals (photos and videos) in full resolution.
- Incremental: a file that is already backed up is never downloaded again.
- A single manual authentication; the session is kept on disk and maintained
  by the service.
- Delivered as a self-contained Docker image (`docker compose up -d`).

## Non-goals

- Native macOS (dropped at the user's request): Docker only.
- Uploading to Amazon, deleting on Amazon, albums, two-way synchronisation.
- A web interface.

## Anti-goals

- Deleting or overwriting a local file because it disappeared or changed on
  Amazon. A backup never loses data.
- Sending session cookies anywhere other than `amazon.<tld>`.
- Failing silently when the session has expired.

## Constraints

- Amazon Photos has no public API. We use the website's internal API
  (`https://www.amazon.fr/drive/v1`), authenticated by the browser's cookies.
  Undocumented: it can break without notice.
- Search is capped: `offset + limit <= 9999`, 200 items per page.
- "Logged in forever" cannot be guaranteed: Amazon can revoke the session.
  The service must then say so clearly and resume as soon as new cookies are
  provided.

## Approaches considered

1. **Browser cookies + internal API** (chosen). The user pastes, once, the
   `Cookie` header copied from their browser. Simple, no embedded browser, an
   image of a few MB.
2. **Headless browser in the container** signing in with username and
   password. Rejected: Amazon's captcha and 2FA make automated sign-in
   fragile, the password would have to be stored, and the image would weigh
   about 1 GB.
3. **Registering as a device** (refresh token, mobile-app style). Rejected:
   it relies on reverse-engineered Amazon application identifiers, with no
   certainty that the Photos web API accepts those tokens.

## Design

Builder's choices (not decided by the user): **Go, standard library only**,
static binary, `distroless` image.

**Commands**: `run` (daemon, default), `login` (imports cookies from stdin or
a file, validates them, saves them), `once` (a single pass), `status`,
`healthcheck`.

**Session**: cookies are stored in `/data/state/cookies.json` (mode 0600). A
purpose-built cookie jar only sends cookies to `amazon.<tld>` hosts, applies
the `Set-Cookie` headers it receives and writes them back to disk. Every cycle
starts with a visit to `/photos/` to keep the session alive. The daemon
re-reads the file when `login` has changed it.

**Inventory**: `GET /drive/v1/search` with the filter
`type:(PHOTOS OR VIDEOS)`. If the total exceeds 9999, it is split by
`timeYear`, then `timeMonth`, then `timeDay`. The sum of the slices is
compared with the overall total; any gap is logged and filled with the 9999
most recent items.

**Download**: `GET /drive/v1/nodes/{id}/contentRedirection?querySuffix=?download=true&ownerId=…`,
written to a temporary file, size and MD5 checked against the metadata, then
an atomic rename and modification time = date taken. 4 parallel downloads by
default, retries with backoff.

**Layout**: `/data/photos/YYYY/MM/<original name>`. On a name collision, a
`_<first 8 characters of the id>` suffix. If an item's content changes on
Amazon, the new version is added next to the old one.

**Index**: `/data/state/index.json` (id → path, md5, size), written
atomically. If the index is lost, a file already present with the right MD5
is re-adopted without downloading it again.

**Expired session**: detected by HTTP 401/403, a redirect to `/ap/signin`, or
an HTML answer in place of JSON. The daemon switches to `auth_required`,
sends a notification (`NOTIFY_URL` webhook, optional), marks the container
`unhealthy`, and resumes as soon as `login` has provided new cookies.

**Configuration** (environment variables): `SYNC_INTERVAL` (6h),
`DOWNLOAD_CONCURRENCY` (4), `BACKUP_DIR`, `STATE_DIR`, `AMAZON_TLD`
(derived from the cookies), `USER_AGENT`, `NOTIFY_URL`.

## Verification

Go tests against a fake Amazon server (paging, the 9999 cap, download
redirects, cookie rotation, session expiry). Validation against the real
service can only happen on the user's side.
