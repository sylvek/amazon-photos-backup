# Amazon Photos Backup Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A Docker-packaged daemon that incrementally downloads a whole Amazon Photos library to a local directory and keeps its web session alive across restarts.

**Architecture:** One static Go binary. `internal/amazon` talks to the undocumented web API (`/drive/v1`) with browser cookies held in a persistent, host-restricted cookie store. `internal/backup` turns the remote inventory into files on disk through an on-disk index. `main.go` wires commands, the daemon loop, status and notifications.

**Tech Stack:** Go 1.24, standard library only. `gcr.io/distroless/static-debian12:nonroot` runtime image. Docker Compose.

**Spec:** `docs/superpowers/specs/2026-11-26-amazon-photos-backup-design.md`

## Global Constraints

- Standard library only; no third-party Go modules.
- Cookies are sent only to `amazon.<tld>` and its subdomains, never to redirect targets on other hosts.
- Never delete or overwrite an existing backed-up file.
- Search paging: `limit=200`, `offset + limit <= 9999`.
- Base query params on every `/drive/v1` call: `asset=ALL`, `tempLink=false`, `resourceVersion=V2`, `ContentType=JSON`. Header `x-amzn-sessionid: <session-id cookie>`.
- State files are written atomically (temp file + rename), `cookies.json` with mode 0600.
- Defaults: `SYNC_INTERVAL=6h`, `DOWNLOAD_CONCURRENCY=4`, `BACKUP_DIR=/data/photos`, `STATE_DIR=/data/state`.

## Review Focus

1. Remote file names containing `/`, `..`, control characters or nothing at all must never write outside `BACKUP_DIR` (test in Task 4: `TestTargetPathSanitizes`).
2. A download cut off mid-stream must leave no final file and no index entry (Task 4: `TestSyncTruncatedDownloadLeavesNoFile`).
3. Session expiry in the middle of a sync must stop the run, keep everything already downloaded, and surface as an auth error (Task 4: `TestSyncStopsOnAuthError`).
4. Two different remote items with the same name in the same month must both be kept (Task 4: `TestSyncNameCollision`).
5. A pasted cookie string with a `Cookie:` prefix, surrounding quotes or trailing newline must still parse (Task 1: `TestParseCookiesVariants`).

---

### Task 1: Cookies — parsing and persistent store

**Files:**
- Create: `go.mod`, `internal/amazon/cookies.go`, `internal/amazon/store.go`
- Test: `internal/amazon/cookies_test.go`, `internal/amazon/store_test.go`

**Interfaces — Produces:**
- `func ParseCookies(input string) (map[string]string, error)` — accepts a raw `Cookie` header (optional `Cookie:` prefix), a JSON object `{name: value}`, or a JSON array of `{"name","value"}` objects.
- `func DetectTLD(cookies map[string]string) (string, bool)` — `at_main` → `com`; `at-acbfr` → `fr`; suffix map `uk→co.uk`, `jp→co.jp`, `au→com.au`, `br→com.br`, `mx→com.mx`.
- `func MissingCookies(cookies map[string]string) []string` — required: `session-id`, plus `ubid-acb<xx>`/`at-acb<xx>` or `ubid_main`/`at_main`.
- `type Store`; `func NewStore(path string) *Store`; `(*Store) Load() error`; `(*Store) Replace(map[string]string)`; `(*Store) Save() error`; `(*Store) ReloadIfChanged() (bool, error)`; `(*Store) Get(name string) string`; `(*Store) Len() int`; `(*Store) Jar(domain string) http.CookieJar`.

- [ ] Tests: `TestParseCookiesVariants` (header, prefixed, quoted, JSON object, JSON array, empty → error), `TestDetectTLD`, `TestMissingCookies`, `TestStoreRoundTrip` (file mode 0600), `TestJarOnlySendsToDomain` (`www.amazon.fr` gets cookies, `evil.example` and `notamazon.fr` get none), `TestJarAppliesSetCookie` (new value stored, `Max-Age<0` removes), `TestStoreReloadIfChanged`.
- [ ] Run `go test ./internal/amazon/` — fails (undefined).
- [ ] Implement.
- [ ] Run — passes.

### Task 2: API client — requests, auth detection, retries

**Files:**
- Create: `internal/amazon/client.go`, `internal/amazon/amazontest/fake.go`
- Test: `internal/amazon/client_test.go`

**Interfaces — Produces:**
- `type Options struct { BaseURL, UserAgent string; Store *Store; Retries int; RetryBase time.Duration }`
- `func NewClient(o Options) (*Client, error)` — cookie domain is the host of `BaseURL` minus a leading `www.`.
- `var ErrAuth error`; errors wrap it when: status 401/403, a redirect whose path starts with `/ap/signin`, or a 200 that is not JSON where JSON was expected.
- `type Node struct { ID, Name, Kind, Status, OwnerID, CreatedDate, ModifiedDate string; Content ContentProperties }` (`json:"contentProperties"`), `type ContentProperties struct { MD5 string; Size int64; ContentType, Extension, ContentDate string }`.
- `(*Client) Root(ctx) (Node, error)` — `GET /drive/v1/nodes?filters=isRoot:true`, first element of `data`.
- `(*Client) KeepAlive(ctx) error` — `GET /photos/`.
- `amazontest.Fake`: an `httptest`-backed fake with fields `Nodes []amazon.Node`, `Content map[string][]byte`, `ValidAT string`, `RotateSessionID bool`, `FailDownloads map[string]int`, `Truncate map[string]bool`; `New(t) *Fake`; `URL() string`; `CDNURL()`. Implements `/drive/v1/nodes`, `/drive/v1/search` (filters `type`, `timeYear`, `timeMonth`, `timeDay`; 400 when `offset+limit>9999`), `/drive/v1/nodes/{id}/contentRedirection` (302 to a second server that fails the test if it receives a `Cookie` header), `/photos/`.

- [ ] Tests: `TestRootOK` (asserts base params and `x-amzn-sessionid`), `TestAuth401`, `TestAuthSigninRedirect`, `TestAuthHTMLInsteadOfJSON`, `TestRetryOn503ThenOK`, `TestSetCookieRotationPersisted`.
- [ ] Fail → implement → pass.

### Task 3: Inventory and download

**Files:**
- Create: `internal/amazon/list.go`, `internal/amazon/download.go`
- Test: `internal/amazon/list_test.go`, `internal/amazon/download_test.go`

**Interfaces — Produces:**
- `type ListReport struct { Total, Listed, Unreachable int }`
- `(*Client) ListAll(ctx) ([]Node, ListReport, error)` — base filter `type:(PHOTOS OR VIDEOS)`, sort `['createdDate DESC']`, `searchContext=customer`, `lowResThumbnail=true`. Total ≤ 9999 → page directly. Otherwise query each `timeYear:(Y)` for Y in 1900..currentYear+1; a year over 9999 splits into `timeMonth:(1..12)`, a month over 9999 into `timeDay:(1..31)`; a day over 9999 keeps its first 9999. Then, if fewer distinct ids than `Total`, add the unfiltered first 9999 and dedupe by id. `Unreachable = Total - Listed`.
- `(*Client) Download(ctx, n Node, w io.Writer) (int64, error)` — `GET /drive/v1/nodes/{id}/contentRedirection` with `querySuffix=?download=true` and `ownerId` (node's, else root's cached). Non-2xx is an error; the caller verifies MD5.

- [ ] Tests: `TestListSmallLibrary` (450 nodes → 3 pages), `TestListPartitionsByYear` (12 000 nodes over 3 years → all listed, no 400), `TestListSplitsBigYearByMonth` (one year with 15 000), `TestListRecoversUndatedViaFallback` (nodes the year filter misses), `TestDownloadFollowsRedirectWithoutCookies`, `TestDownloadAuthError`.
- [ ] Fail → implement → pass.

### Task 4: Backup engine — paths, index, sync

**Files:**
- Create: `internal/backup/paths.go`, `internal/backup/index.go`, `internal/backup/sync.go`
- Test: `internal/backup/paths_test.go`, `internal/backup/sync_test.go`

**Interfaces — Consumes:** `amazon.Client.ListAll`, `.Download`, `amazon.ErrAuth`, `amazontest.Fake`.
**Produces:**
- `func TargetPath(n amazon.Node) string` — `YYYY/MM/<sanitized name>` from `Content.ContentDate`, else `CreatedDate`, else `unknown/`. Sanitizing: path separators and control characters become `_`, leading dots stripped, empty name becomes `<id>` plus extension, at most 200 bytes.
- `type Index`; `LoadIndex(path) (*Index, error)`; `(*Index) Save() error`.
- `type Result struct { Remote, Downloaded, Skipped, Adopted, Failed, Unreachable int; Bytes int64 }`
- `type Syncer struct { Client *amazon.Client; Dir string; Index *Index; Concurrency int; Log *slog.Logger }`; `(*Syncer) Run(ctx) (Result, error)` — returns an error wrapping `amazon.ErrAuth` as soon as any call hits one.

Per node: indexed with same MD5 and file present with same size → skip. Indexed with a different MD5 → new path with suffix `_<first 8 of id>` (again suffixed with a counter if taken); the old file and entry stay. Not indexed and target exists with matching MD5 → adopt. Otherwise download to `<path>.part`, verify size and MD5, rename, set mtime. Index saved every 50 downloads and at the end.

- [ ] Tests: `TestTargetPathSanitizes`, `TestSyncDownloadsThenSkips` (second run downloads 0), `TestSyncNameCollision`, `TestSyncChangedContentKeepsOld`, `TestSyncAdoptsExistingFile`, `TestSyncTruncatedDownloadLeavesNoFile`, `TestSyncBadMD5Fails`, `TestSyncStopsOnAuthError`, `TestSyncSetsMtime`.
- [ ] Fail → implement → pass.

### Task 5: Daemon, commands, status, notifications

**Files:**
- Create: `main.go`, `internal/daemon/daemon.go`, `internal/daemon/status.go`, `internal/daemon/config.go`
- Test: `internal/daemon/daemon_test.go`

**Interfaces — Produces:**
- `type Config struct { BackupDir, StateDir, BaseURL, TLD, UserAgent, NotifyURL string; Interval time.Duration; Concurrency int }`; `func ConfigFromEnv() (Config, error)`.
- `type Status struct { State string; LastRunStart, LastSuccess time.Time; LastError string; LastResult backup.Result }` with states `starting`, `syncing`, `ok`, `error`, `auth_required`; stored in `<StateDir>/status.json`.
- `func Login(cfg Config, r io.Reader, w io.Writer) error`; `func Once(ctx, cfg, log) (backup.Result, error)`; `func Run(ctx, cfg, log) error`; `func Healthcheck(cfg Config, now time.Time) error` — unhealthy on `auth_required`, or when the last success is older than `3 × Interval` and the daemon is not in its first sync.
- Commands in `main.go`: `run` (default), `login [--file path]`, `once`, `status`, `healthcheck`.

Loop: reload cookies if the file changed → keep-alive → sync → save cookies and status → sleep `Interval`. In `auth_required`, poll the cookie file every 30 s and resume on change. One notification per transition into `auth_required` or `error` (HTTP POST, plain-text body, to `NOTIFY_URL`).

- [ ] Tests: `TestLoginValidatesAndSaves`, `TestLoginRejectsBadCookies`, `TestRunRecoversAfterNewCookies`, `TestNotifyOncePerTransition`, `TestHealthcheck`.
- [ ] Fail → implement → pass. `go vet ./...` clean.

### Task 6: Packaging and documentation

**Files:**
- Create: `Dockerfile`, `docker-compose.yml`, `.dockerignore`, `README.md` (French)

- [ ] Multi-stage build: `golang:1.24-alpine` → `distroless/static-debian12:nonroot`; `/data/photos` and `/data/state` owned by uid 65532; `VOLUME /data`; `HEALTHCHECK` using the `healthcheck` command.
- [ ] Compose: `restart: unless-stopped`, volume for `/data`, env defaults.
- [ ] README: how to copy the `Cookie` header, `login` via stdin pipe, start, check status, renew the session, limits (unofficial API, session can be revoked).
- [ ] Verify: `docker build` succeeds and the container syncs from the fake server end to end, if a Docker daemon can be started here; otherwise say so.
