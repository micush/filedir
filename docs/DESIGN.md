# filedir design

filedir distributes files between the nodes of a cluster. Any node can be written to; every node can be read from. It is a
distribution system, not a file server. One binary, standard library only.

## Sync engine (`source/filesync`, done)

- **Index.** Each folder keeps an index of entries (file, directory, symlink, or tombstone) in a journal outside the folder. Every
  entry has a version vector, a sequence number (what a peer asks "after"), and for files a list of block hashes.
- **Scan.** Quick check by size, mtime and mode; rehash on change. A missing path gets a tombstone, except under unreadable
  directories. The folder must hold `.filedir/` (the marker), or nothing is scanned: an unmounted disk is not a mass deletion. A scan
  that would delete more than half of 100+ files is refused.
- **Compare.** Equal or older: ignore. Newer: take it. Concurrent: a change beats a deletion; otherwise the later mtime wins, then the
  higher node ID. The loser is kept as `name.sync-conflict-DATE-TIME-NODE.ext`. The winner is stored under a version that merges both
  and adds one for the node that settled it (two nodes settling two different pairs must not produce the same vector; found by the
  soak test).
- **Swarm pull.** A file is built in `.filedir/tmp` from blocks. Blocks already on disk are reused; the rest are fetched in parallel,
  each from a different peer that holds that version (`Config.Peers`), falling back to the next peer on failure. Every block is
  checked against its hash. The more nodes hold a file, the faster it spreads, as in BitTorrent. (Not yet: serving blocks of a file
  that is still downloading; rarest-first ordering.)
- **Safety.** Names from peers are validated (no `..`, absolute paths, `.filedir`, empty parts, NUL). A path is refused if a parent is
  a symlink. A file changed locally since the last scan is never overwritten. Replaced and deleted files go to `.filedir/trash`
  (30 days); tombstones are kept 180 days. Partial files are never visible and are removed at start.
- The engine does no networking. The caller gives it a `Source` per peer (`Index`, `Block`).

## Transport and cluster (done)

- Forked from logdir: cluster join and trust, gateways and VIP, users, MCP, updates, backups, web GUI. The log collector was removed.
- Ports: GUI 4450, cluster 4451, files 4452. Gateway ids start at 137.
- `fd_cluster.go` runs one `filesync.Folder` per configured folder. It scans every 30 s, and at once when `fd_watch.go` (inotify, one watch per directory) sees a change; a scan that found changes sends
  `/cluster/files/notify` to the other nodes, which then sync from the sender straight away. It syncs from every reachable peer every 10 s and
  runs maintenance (trash, tombstones). `fdSource` implements `Source` over a dedicated TLS listener on :4452. It uses the cluster
  identity: pinned certificates and signed requests, the same trust as the cluster port. `ClusterPeer.FilePort` carries each node's
  file port. A view of what each peer holds (`holders`) feeds `Config.Peers`, so a fetch spreads over every node that has the version.
- Routes on :4452: `/cluster/files/index`, `/cluster/files/block`, `/cluster/files/status`, `/cluster/files/notify`.
- File manager API (`fd_files_api.go`): download, zip (`filesync.WriteZip`, streamed, links and the metadata folder left out), text (1 MB), upload (raw body), mkdir, rename, delete, list (`dirs=1` for the tree). Writes go through the engine's
  atomic `WriteFile` and trigger a coalesced scan. Read-only accounts are refused every POST.
- MCP: `files_status`, `files_list`, `files_read` (a chunk at a time, text or base64, sized to half the result limit) and `files_write` (rw keys only: one call is the 1 MB request limit, so a large file is sent in parts through `Folder.StageAppend`/`StageCommit`, staged under `.filedir/tmp` and committed atomically as one change; a stage nobody finishes is removed after an hour). The only tool that changes anything.

## Still to build

- Serve blocks of a file that is still downloading, and rarest-first ordering, so a swarm of many nodes starts faster.
- MCP `files_read`.
- Per-folder permissions for the files site (every signed-in end user sees every folder), and end-user accounts on the Users page (they are system or directory accounts).

## The files site


**WebDAV** (`fd_webdav.go`) is hand-written over `net/http` on the files site mux at `/dav/` (the standard library has none). Clients cannot use the cookie session, so it takes HTTP Basic: `verifyCreds` (extracted from `login`) is the one check of name and password (PAM, then LDAP, then `filesAccess`), with the same lockout keys and login queue; a good sign-in is remembered for a minute (a salted hash of name and password, in memory) because a client sends it with every request. SSO-only people have no password and cannot use it. Class 1 and 2: PROPFIND (depth 0 and 1; infinity is refused), PROPPATCH (accepted, not stored), GET/HEAD, PUT, DELETE, MKCOL, MOVE, COPY, and LOCK/UNLOCK that hand out a token and enforce nothing (Finder and Office only need the answer). It reads the disk (`Folder.Stat`, `Folder.ReadDir`) and not the index, which can be a scan behind a write. A read-only person gets only OPTIONS, GET, HEAD and PROPFIND.

**Transfer speed.** A file is pulled block by block (`Folder.fetch`: 4 workers per source, at most 16; blocks of 128 KB, growing so that a file has at most about 2000 of them). The block calls go through `callAddrWith(keep=true)` and a pooled HTTP client per peer address and identity (`pooledClientAs`), so a connection is reused; every other cluster call still uses a fresh connection (`pinnedClientAs`), so a control call never waits on a dead one. The kept-connection calls only ask, and are marked repeatable so the transport resends one that finds its connection closed. `TestFilesTransferRate` logs the rate between two nodes on one machine.

**Bandwidth limits** (`fd_limit.go`, `FilesConfig.Limit*Mbit`): three package-level `fdLimiter`s (uploads, downloads, replication), per node, set from the shared settings at every `reconcile`. A limiter keeps a schedule: a transfer asks for the time its bytes take at the rate and sleeps until then, at most 100 ms behind the clock (a small file goes at once), in chunks of 32 KB so a low rate is smooth. Reads (an upload body, a fetch) and writes (a download, a zip, the blocks a node serves to its peers) go through `fdThrottleReader` / `fdThrottleWriter`; the writer passes `Unwrap` so the deadlines can still be lifted. Replication is limited by the node that serves the block, and the node that asks allows a second more per 128 KB before its time limit. Per node, not per cluster.

**The disk limit** (`filesync/diskcap.go`, `FilesConfig.MaxDiskPct`, default 80, 100 = none): `Folder.room` compares what the filesystem under the folder would be after the write to the limit, `df`-style (used over used plus what an ordinary user may still use). It is asked before `WriteFile`, `StageAppend` and `pullFile` (the size is known), and a `guard` reader asks again every 4 MB of a write of unknown length. A change of the limit reopens the folders.
`webfiles.go` adds routes beside `/mcp` on the gateway listeners (`mcplisten.go`): the page `files.html`, `app.js`, `style.css`, login, logout, session and the files API, and nothing else. It is always on: the people of `web.group` and `web.read_group` may sign in at the same level, and `web.files_group` and `web.files_read_group` are the files-only groups (`filesAccess`); the directory looks for the same names (`WebConfig.ldapFor`, joined with newlines so a DN is not cut; `ldap_rw_group` and `ldap_ro_group` remain only as an override for the management GUI); it is always served, and with no group at all nobody can sign in. A download link opened without a session is redirected to the sign-in (`/?next=`) and on to the download afterwards. Directory (LDAP) and single sign-on users that the management GUI takes sign in here too, at the same level (`login` and `ssoStart`/`ssoCallback` take the site; a files-site SSO sign-in returns to the files site's own host, which must be a registered redirect address). Public links (`fd_publink.go`) are `/dl/<token>/<name>`, a signed (HMAC with a key derived from the cluster secret) folder, path and expiry of 1 to 30 days: no state, valid on every node, not revocable before they expire. Upload from URL (`fd_fetch.go`) fetches http and https only and refuses loopback, link-local, multicast and unspecified addresses at connect time (after name resolution and every redirect). Sessions have `site: files` and their own cookie (`__Host-filedir_files`); `lookupSite` refuses a session of the other site. `websessshare.go` shares them: `put` on login, on a change of access and as activity (at most every 20 s), `del` on logout (a tombstone keeps a late `put` from reviving it), and `get` when a node meets an unknown cookie (rate limited and negatively cached, so made-up cookies do not load the cluster). The records carry the hash of the cookie, never the cookie. Messages go over `/cluster/websession` on the cluster port. A node that was down when someone signed out can still hold that session until it expires.
- `reconcile()` does not retry a folder whose Open failed until the config changes.
- Real PAM sign-in has not been checked on a machine (the tests use a fake).
