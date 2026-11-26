# Changelog

## [114]

### Fixed
- **WebDAV at `/dav` (no slash) works.** It was answered with a redirect to `/dav/`, which KDE Dolphin and some other clients do not follow for a listing, so `davs://host/dav` said the folder does not exist. It is now answered as `/dav/` is.
- The README said the files site has no directory login. It does: a directory account in one of the groups signs in there too, nested groups included.

## [113]

### Changed
- **Scheduler: a script task runs a script from a folder on the node, not a command typed in the page.** The "Run a shell command" task is now "Run a script". Its setting is the name of a file in `/etc/filedir/tasks/` on each node that runs it, then its arguments (`'single'` or `"double"` quotes for an argument with spaces). The file is run directly: there is no shell and nothing is expanded. Before, the command text was edited in the web page, copied to every node and run with `/bin/sh -c`, so anyone who could edit tasks could run any command as the service user on every node. Now a task can only run what an administrator has put on that node. The folder and the file must be owned by root (or the user filedir runs as), the file a plain file (not a link) that its owner can run, and neither writable by group or others; a name with a folder in it is refused when the task is saved. CodeQL's "Command built from user-controlled sources" alerts (#52, #53) pointed here.
- **A task that holds an old inline command stops working until it is moved.** Put the command in a script in `/etc/filedir/tasks/` (for example `#!/bin/sh` and the old command line), make it `chmod 755` and owned by root, and set the task to the script's name. The run history shows "there is no script … in /etc/filedir/tasks on this node" until then.

### Fixed
- The PAM service name is looked up in the listing of `/etc/pam.d` instead of being joined into a path (alert #118, #107).

### Not changed
- The sync folder's own paths (alerts in `filesync/index.go` and `folder.go`) are the folder an administrator chose to sync; opening files under it is the feature. Those are for dismissing as false positives.
- Fetch from URL (alert #51) is as it was: the person types the address on purpose, and the node already refuses private, loopback and link-local addresses at connect time, after redirects too. Nothing short of removing the feature satisfies CodeQL there, so that alert is for dismissing as a false positive.

### Verified
- gofmt, `go vet`, the scheduler tests (a script run with arguments and no shell, the time limit, the ignored exit status, and every refusal: a name with a path, a missing script, a file or folder anyone can write to, a link, a file that cannot be run). The page's syntax was checked; it was not opened in a browser.

## [112]

### Changed
- **A name from the settings is checked before it is put into a path.** The service name for sssd (`/etc/sssd/sssd.conf.<service>.bak` and `.tmp-<service>`), the PAM service name (`/etc/pam.d/<name>`) and the interface names for the kernel's ARP and reverse-path settings (`/proc/sys/net/ipv4/conf/<name>/…`) may now hold only letters, digits and `. _ : @ -` (at most 64, and not `.` or `..`). Anything else is refused or skipped, and said so in the log, so a setting can never reach outside the folder it is meant for. These are the places CodeQL's "uncontrolled data used in path expression" alerts pointed at; none was reachable from a request, so this is hardening and no behaviour changes for any name in use.

### Verified
- gofmt, `go vet`, the full test suite, a new test for the name check (`pathname_test.go`) and `GOOS=linux go build` for amd64, arm64, arm, 386 and riscv64. CodeQL 2.27.6 with the Code Scanning queries was run on filedir: it reports five findings (a TSIG key path in the DNS registration settings, the Fetch-from-URL request, two certificate-check switches), all by design. It did not reproduce the other alerts GitHub lists, so whether they close can only be seen after the next GitHub scan.

### Not verified
- Not run against a live cluster.

## [111]

- Operate ▸ Updates: Fetch from online is ticked when the page opens, so Upload fetches the newest release from GitHub. Untick it to choose an archive file.

## [110]

- Operate ▸ Updates: the Nodes table is laid out with set column widths: the node name gets the most room, the two versions are narrow and sit side by side, then Reachable, Queued and State at an even spacing. Long names are cut with an ellipsis, not pushed into the next column.

## [109]

- Operate ▸ Updates: the Nodes table has equal-width columns, all left-aligned, so the space between them is the same.

## [108]

- Operate ▸ Updates: even more space between Source and Reachable.

## [107]

- Operate ▸ Updates: more space between the Source and Reachable columns in the Nodes table.

## [106]

- Operate ▸ Updates: in the Nodes table the Running and Source versions come right after the node name, ahead of Reachable.

## [105]

- Files: the Delete confirmation says "The file is moved to the trash and can be restored if needed."

## [104]

- Files: the Shift+Delete confirmation is titled "Permanently delete NAME?" and its button says Delete.

## [103]

- Files: the Shift+Delete confirmation is shorter: "The file is deleted everywhere and cannot be restored."

## [102]

- Files: Shift+Delete now deletes everywhere with no copy kept. In v101 the other nodes still put their copy in their trash. A deletion made this way is marked in the index entries (`purge`), and every node it reaches removes the file at once instead of moving it to the trash. A node on an older version ignores the mark and keeps its copy in its trash as before.

## [101]

- Files: Shift+Delete deletes for good. The confirmation says so, and this node keeps nothing in its trash. The deletion still reaches the other nodes, which keep their own copy in their trash for 30 days as before. Plain Delete is unchanged.

## [100]

- Files ▸ Speed test: the card was empty (or missing) after leaving the Files page and coming back. The page remembered which nodes it had drawn the card for, and on coming back it saw nothing had changed and did not draw it into the new card. It is drawn every time the page opens now. This is the cause of the card that came and went in v87–v99; found by reproducing it in a headless browser with a status like the real one and switching away and back.

## [99]

- Files ▸ Speed test: the card is always shown. With fewer than two nodes sharing a folder it says what it needs, in place of disappearing and coming back.

## [98]

- A folder dropped on the Files page no longer stops at the first file the folder's ignore list covers (such as one inside `.git/`). The node now answers "skipped" for such a file or folder, nothing is written, and the upload goes on with the rest; the page says at the end how many were left out. Before, the refusal was an error and ended the whole upload.

## [97]

- Ignore patterns now cover everything under an ignored folder. Before, a pattern such as `.git/` was matched against the folder itself only, so a file inside it that arrived from another node (or an upload) was accepted, never indexed here, and then failed with "the file changed here since the last scan". Such files are now left out in every direction, like the folder.

## [96]

- Configure ▸ Folders: the folders table now runs the full width of the card, so its right edge lines up with the fields under it.

## [95]
- The Speed test card no longer goes away for a moment now and then. It showed only while the last status named two nodes, and a node that is busy (with the test itself, or an upload) can miss a status: the page then had one node, hid the card, and showed it again at the next status that had both. A node now stays in the card's lists for three minutes after the last status that named it.

## [94]
- An upload of tens of thousands of files no longer starts a scan of the whole folder for every file. After each change made through the web page (an upload, a delete, a rename) the node asked for a scan, and the check that was meant to keep them from piling up was cleared before the scan began, so with a file every few milliseconds there were hundreds of scans waiting to run one after another, each walking all the files, on the same disk that was trying to write the uploads. Requests took a minute or more ("waiting for the node, 77 s") and the upload crawled. Now there is one scan at a time: the first one starts at once as before, and changes that come while it runs (or waits) are taken by one more scan, which starts after a pause of twice as long as the last scan took (at least a second), so scanning never takes more than a third of the time.

## [93]
- An upload sends twelve files at once, not three (never more than three big ones among them). Every small file costs a round trip and a write on the node, so three at a time left the node waiting. Measured against a node with nothing between (1,500 files of 20 KB, two cores): 3 at a time 775 files a second, 8 at a time 912, 16 at a time 999, 32 at a time 1,219. Over a line with some distance the gain is larger, since each request then waits longer for its answer.

## [92]
- An upload that sat at "79,654 of 79,655" for ever: one request had gone quiet and nothing in the page ever gave up on it. Every request of an upload (a small file, or a piece of a big one) is now stopped after two minutes without an answer and sent again (up to five times, then the upload stops and names the file). And the footer says which file is holding the upload up ("path: waiting for the node, 40 s") once one has taken more than ten seconds, redrawn every two seconds, so a stuck upload says what it is waiting for. Making a folder is not covered by the two minutes yet.

## [91]
- Dropping a folder of many files on the window shows something at once. The browser hands a folder over entry by entry and each file only when asked, and the page asked for them one after the other, which took some twenty seconds for tens of thousands of files with nothing on the screen. They are asked for sixty at a time, and the footer reads "Reading the folder… N files" with a ✕ while it counts, then turns into the upload line.

## [90]
- The red ✕ of an upload, a fetch from a web address, and a copy or move cancels when the pointer goes down, not when the button comes up, and only once (the keyboard still works through the click). A line redrawn while a transfer runs could take the click away, and the count went on. Reported on an upload of 78,346 files: pressing ✕ did nothing.

## [89]
- The red ✕ on the line of an upload cancels the whole upload again when it is a big one. With thousands of small files the line was made anew after every file, so the ✕ was gone from under the pointer between pressing and letting go of the mouse button and the click did nothing (or was lost with the file that happened to end then). The line and its ✕ are now made once, and only its text changes, at most four times a second. (A click on it stops what is going up and starts nothing more, as before.)

## [88]
- The speed test is its own card on the Files status page, titled "Speed test": pick the node that sends and the node that receives, press Test, and the result is shown as a headline speed, what limits it (the channel, the hash or the disk) and a bar for each part and for the whole path. The test is run on the receiving node and asks only the node picked as the sender.

## [87]
- Fixed the speed test's whole-path run, which failed with "unexpected EOF" on every node: it asked for 128 MB in one request and a response is read only to 33 MB. A stream's share now comes as requests of 32 MB. Replication was not affected (its blocks are 16 MB at most).

## [86]
- Test speed writes 2 GB for the whole path (and for the disk line), not 512 MB: with a node's memory of a few GB, a small write is taken by the cache and the disk is never asked to keep up, which a big file's replication meets. On two nodes the whole path (hash and disk) of 512 MB went as fast as the channel (5.2 against 5.4 Gbit/s) while the disk alone, synced, wrote 260-450 MB/s, which is where the replications of big files stopped. It needs room for twice the 2 GB, and leaves a node out of the test when 60 s have gone (a relayed request is cut off at 90 s).

## [85]
- Test speed measures the whole path for each other node, as well as the channel alone: it fetches the data again as a replication does, with every block of 4 MB checked against its hash (BLAKE3) and written to a file in the first folder's own directory, which is synced at the end and removed. The line for a node reads "From A to B: 6,181 Mbps over the channel, 3,100 Mbps with the hash and the disk". The data is now the same on every node (made from a fixed seed), so the receiver can check it; a block that comes wrong is reported. A node with less than four times the test's size of room on its disk skips the second part and says so. This and the other numbers vary a good deal from run to run on hosts that are shared (BLAKE3 on one node went 874, then 290 MB/s), so run it a few times.

## [84]
- Test speed is a link, not a button, and follows the Node menu: it tests the node that is picked (this one when none is), asking it through the relay like the trash, and says which node each line is about ("From filedir2 to filedir1", "Hashing on filedir1"). The result is kept for the node it was made on and shown only when that node is the picked one. `/api/files/speedtest` may be relayed (proxy.go, and the test of what may be).

## [83]
- **Test speed**, a button in the Files status page's Replication card (`POST /api/files/speedtest`, and `/cluster/files/speed` between the nodes). A replication that is slower than the network does not say why; this measures the parts apart, on the node you are signed in to as the receiver: the data from each other node over the same signed, encrypted file channel with nothing else in the way (sent from memory, thrown away on arrival: no disk, no hash, 16 requests at once as a replication makes), then how fast one core of this node hashes a block (BLAKE3, and SHA-256 for comparison) and how fast its disk takes a write (about 512 MB, synced, in the first folder's own directory, removed after). Compare the first with a real transfer: if the channel is fast and a replication is not, the time goes to the disk or the hash. It needs version 83 on the other nodes too. Asked for after two nodes on a 6.5 Gbit/s network stayed at 2.5 Gbit/s, with no way to see where.

## [82]
- **Blocks are hashed with BLAKE3, not SHA-256. Update every node; nodes of different versions do not sync with each other.** Found by looking at why two nodes stopped at 2.2 Gbit/s on a 6.5 Gbit/s network: the receiving node's cores were at 80-85% user time, and its CPUs (Xeon X5690, 2010) have no SHA instructions, so SHA-256 ran in software, at about 290 MB/s a core. BLAKE3 (package `filesync/blake3`, written here: the standard library only, as the rest of filedir) runs at 1.2 GB/s a core with SSSE3 vectors, which every x86-64 CPU since 2006 has, and at 1.9 GB/s with AVX2, against SHA-256's 1.4 GB/s on a CPU that has the SHA instructions: nothing gets slower. A processor of another kind uses the same algorithm in plain Go (about 215 MB/s). The vector code (hash4_amd64.s, hash8_amd64.s) is made by gen_amd64.py; it hashes four or eight chunks of the message at once, and the same for the parent nodes of the tree.
- **The conversion.** An index written by an older version is converted once, when the folder is first opened: every file in it is read, and the hash of each of its blocks is made again, the blocks cut where the entry cut them. A file counts as what the index says if its size and time are (all a scan ever goes by); one that is not is left for the first scan, which makes a new version of it. Versions, sequence numbers and what has been read of the other nodes are kept. The folder shows "Converting the index to the new block hash: N of M files" meanwhile (as its status) and opens when it is done; the time is that of reading the folder's files once.
- **Mixed versions.** A node asks the others for the index with the hash it uses, and a node that does not say (version 81 or older) is refused with a message that says to update it; an older node that is asked says nothing of it, which the new one takes as the old hash and says so. Nothing syncs between the two until both are updated; nothing is lost or mixed up.
- The tests check BLAKE3 against the reference implementation (Python `blake3`) on every number of chunks up to 70 with the edges of the last, and on sizes up to 16 MB, in each of the three code paths; the conversion, a file that changed before it, the service opening a folder after it, and the refusal of an old node are tested.

## [81]
- Replication moves blocks with less CPU: the buffers a block is read into, sent from and received into come from a pool and go back to it, instead of a new multi-megabyte buffer (cleared, then swept by the collector) for every block; a batch of small blocks is sent as it is read and not copied into one buffer first. Found by looking at why two nodes stopped at 2.2 Gbit/s: the receiving node's cores were at 80-85% user time (old CPUs without the SHA instructions).

## [80]
- The replication graphs are in Mbps (megabits a second), like the replication limit.

## [79]
- The empty-trash question's buttons are just "This node" and "All nodes".

## [78]
- Files status: the red ✕ next to Trash asks whether to empty the trash on this node or on all nodes (the nodes that have any). With one node with a trash it asks as before.

## [77]
- Files status: the replication graphs sit side by side (they wrap onto a new line only when the window is too narrow).

## [76]
- Files status: the Replication card has a bandwidth graph for each node: what it received from the others and what it sent to them, in bytes a second over the last half hour. Counted as the blocks go by, sampled every 5 s, kept in memory (a restart starts it again).

## [75] - 2026-11-24 — An upload says how far it is

### Fixed
- **An upload only said "Uploading 0 of 1…" until it was over.** The page had the figures of each big file going up (how much of it, in percent) and did not show them. A single file now reads "Uploading name: 1.2 GB of 3.3 GB (36%)" with the stop next to it; several files read "Uploading 3 of 40…" with a line for each big one going up now under it. A single small file shows its name and size.

## [74] - 2026-11-24 — Trash in the Disk space pies, with a red x to empty it

### Added
- **The Disk space pies have a Trash slice**: what the trash of the synced folders holds on that node, apart from the synced files and the rest of the disk (the trash was in "Other files" before). The nodes report it as `trash_bytes` for each folder; it is counted by walking the trash, so the count is at most a minute old (and counted again at once after the trash is emptied, restored from or deleted from), and the slice shows up a few seconds after the page is opened.
- **A red ✕ at the Trash of a pie empties the trash of the folders on that disk of that node.** It asks first (how much it holds, that it cannot be got back, that the other nodes keep their own), and does it through that node when it is not the one signed in to. Not shown to a read-only account.

## [73] - 2026-11-24 — The Delete key

### Fixed
- **The Delete key deletes what is selected in more places.** It worked only with the focus on a row of an ordinary list. Now it also works with the focus on the list itself (after Ctrl+A, or a click on its empty space), on the matches of a filter, and on a folder of the tree (that folder). It asks first, as the menu does, and the files go to the trash of each node.

## [72] - 2026-11-24 — Large icons

### Added
- **The Files list can be shown as large icons**, as in a file manager: the ☰ / ▦ buttons at the right of the title bar switch between the list and the icons, and the browser remembers the choice. It is the same list laid out as tiles, so selecting (click, Ctrl, Shift), the right-click menus, drag and drop, rename, Delete and opening work the same; the arrow keys move a tile left and right and a whole row of tiles up and down. Size, date and the like are in the tooltip of a tile. The column headers (and so sorting) are in the list view.

## [71] - 2026-11-24 — A long error can be read on the status page

### Fixed
- **The scroll bar of the Files table on the status page jumped back as soon as it was moved**, so a long error message at the end of a row could not be read. The page redraws every few seconds and put the table back to the left each time; a table that was scrolled now keeps its place.
- A long error in the Sync column wraps onto lines of its own instead of running off the card.

## [70] - 2026-11-24 — A pull that was cut off carries on where it stopped

### Fixed
- **A big file that was part-way across when the network went down started again from block 0.** The partly pulled file was deleted whenever a pull failed. It is now kept in `.filedir/tmp/part-…` (named by the file and its version) and the next try checks it block by block against the hashes, keeps the blocks that are right and fetches only the rest; a block that is wrong in it (a crash, a damaged disk) is fetched again. It is removed when the file arrives, when the file was deleted meanwhile, or by the maintenance run 72 hours after the last try. Nothing partial is ever shown in the folder or served as a finished file.

## [69] - 2026-11-24 — Disk space colours are the same in light and dark

### Changed
- The three colours of the Disk space pies (blue, amber, green) no longer change with the theme; they are the ones of the dark theme in both.

## [68] - 2026-11-24 — A red x to stop a transfer; drag folders in the left pane

### Changed
- The Cancel button on the line of an upload, a fetch from a web address, or a copy or move is a red ✕ on its own, with no button drawn around it (its tooltip and its name for a screen reader are still "Cancel").

### Added
- **Folders in the left pane can be dragged onto other folders** (in the left pane or the list) to move them there; hold Ctrl to copy. A folder cannot be dropped into itself or into what is inside it, and the trash is not a place to drop on.

## [67] - 2026-11-24 — Nodes in alphabetical order

### Changed
- On the Files status page the nodes are in alphabetical order of their names in every card (Files, Disk space, Replication: its rows, its columns and the files on their way), instead of the order the cluster answered in.

## [66] - 2026-11-24 — The footer no longer starts with "null"

### Fixed
- The line at the bottom of the Files page began with the word "null" (since [63]): the note "Showing another node's copy" is nothing when you look at your own node, and nothing was written out as that word.

### Changed
- The note itself ("Showing filedir2's copy of the folders …") is gone. Another node's copy is still read only.

## [65] - 2026-11-24 — Disk space: spread out, by node name

### Changed
- The pies of the Disk space card are in alphabetical order of the node names and spread evenly across the card.

## [64] - 2026-11-24 — A backup no longer holds the synced files

### Changed
- **The backup archive left out the settings' size and took the files instead.** It packed the whole state directory, and the synced folders live inside it (`/var/lib/filedir/folders/<id>`), so every archive held every synced file under 256 MB, each folder's index and its trash (30 days of deleted copies, which is why the archives grew by the gigabyte after a big delete). `folders/` is now left out: a backup holds the settings, users, keys and certificates, a few MB. The files are on every node, and a restore leaves a node's own `folders/` alone.
- **Configure ▸ Backup ▸ Contents ▸ "Include the synced files"** puts them back in (`include_files` in the backup settings), for a single node or a copy outside the cluster; files over 256 MB are still left out.

## [63] - 2026-11-24 — Look at another node's copy of the folders

### Added
- **The Node menu picks whose copy of the folders the Files page shows.** Before, the page always showed the copy of the node you were signed in to. Picking another node now lists, searches and reads (the list, the tree, the footer numbers, text files, sizes) the folders as that node has them, which shows a node that is behind. That copy is **read only**: Upload, New, Rename, Delete, Paste and the like are not offered, and the footer says whose copy it is; pick the node you are signed in to (or "this node") to change files. Downloads still come from the node you are signed in to.
- Only reading calls and the trash can be relayed to another node (`/api/files/list`, `search`, `size`, `text`, `status`, `trash`); everything that changes something still acts on the node signed in to.

## [62] - 2026-11-24 — The trash of the node picked in the Node menu

### Fixed
- **The Files page showed the trash of the node you are signed in to, whichever node the Node menu had picked.** The files are the same on every node, so the page ignores the menu, but a trash is each node's own. Its calls (look, list, restore, delete for good, empty) are now sent to the picked node, and the page says whose trash it is. The rest of the Files API is still not relayed.

## [61] - 2026-11-24 — Disk space colours

### Fixed
- The colour boxes in the legend of the Disk space card were empty (the colour was set in an inline style, which the page does not apply); they are CSS classes now.
- The Free slice is green instead of a dark grey that was hard to see in dark mode.

## [60] - 2026-11-24 — No Size column in the Files card

### Changed
- The Files card of the status page no longer has a Size column; the Disk space card shows what the folders hold, with the rest of the disk.

## [59] - 2026-11-24 — Disk space pie charts

### Added
- **Files ▸ Status has a "Disk space" card**: a pie chart for the disk of each node that holds synced folders, with what the folders hold, what else is on the disk, and what is free (df's numbers: free is what an ordinary user may still use). Folders on the same disk share one pie. The node answers with `disk_used`, `disk_free` and `disk_id` for each folder (`GET /api/files/status`).

## [58] - 2026-11-24 — Upload from URL shows the file when it is done

### Fixed
- **A file fetched from a web address waited minutes after the last byte before it showed.** The download was written as it came but not hashed, so the node read the whole file again (a scan) before it was in the list and could start to reach the other nodes. When the server says how big the file is, it is now hashed as it is written and goes into the index at once, as uploads and copies already do. A server that does not say (a chunked answer) still gets the scan.

## [57] - 2026-11-24 — Slow click renames a folder in the left pane

### Fixed
- **A slow second click on the open folder's name in the left pane renames it in place**, as in the right pane.

## [56] - 2026-11-24 — Dialogs, sorting and renaming in the file manager

### Fixed
- **Several transfers at once no longer fight over the footer line.** Each upload, fetch from a web address, and copy or move has its own line (with its own Cancel); before, they overwrote one another, so the text flipped between them.
- **Right-click ▸ Delete works on the matches of the filter** (one or several, also from different synced folders). Rename is still done in the folder itself (Show in folder).
- **Click a column header to sort the list** (Name, Size, Modified, Node; a second click reverses it, ▲/▼ shows which). Folders stay above files. Not in the trash, which stays newest first.
- **Folders in the left pane can be renamed in place**, as files and folders in the right pane can: Rename in the menu, F2 on the focused folder. The open folder and the way to it follow the new name.
- **The folder menus say "Upload", "New" and "Paste"**, not "Upload here", "New here" and "Paste here" (the folder is the one that was right-clicked).
- **Pressing Enter in a dialog with a text box (New folder, New file, Rename, …) closed it without doing anything.** Enter pressed the first button of the dialog, which is Cancel, so the name was thrown away and only a click on the Create / Save button worked. Enter now presses the button that does it.
- **A reload that was asked for while another one was running was dropped.** The file list reloads after every action; if the running reload had read the list before the change, the list stayed as it was until the next refresh. It now runs again as soon as the running one is done.

### Not changed
- "New folder" makes the folder on the disk and asks for a scan, and the list is read from the index, so a folder can take a moment to show (longer while a sync is running). Uploaded files go into the index at once; folders do not yet.

### Verified
- `node --check` of the web UI, gofmt, vet, `go test -race ./...`, the five cross builds. The page was not run in a browser here.

## [54] - 2026-11-24 — A deleted folder tree no longer comes back as empty folders

### Fixed
- **A deleted folder tree came back as a skeleton of empty folders, on every node.** The log of v53 showed why: the kept folders still held files, because the other node's index is read in pages of 5,000 entries and the deletions were numbered folder first, files after. A page that ended between a folder's deletion and the deletions of what was in it handed over the folder first; the folder could not be removed yet, was kept with a newer version, and brought its parent folders back with it, one chain per page boundary. Deletions are now numbered with what is under a folder before the folder itself, so no page can split them (every writer of the index goes through this).
- **A folder with nothing in it no longer beats a deletion.** A folder that had been kept before carried a change of its own and counted as "a change here beats the deletion", so the next deletion of its tree failed the same way. An empty folder now loses to the deletion.

### Verified
- gofmt, vet, `go test -race ./...`, the five cross builds. New tests: a deletion read in pages of 7, 50 and 333 entries (fails without the fix), and an empty folder changed here against a deletion. Not checked on the cluster. Folders left over from earlier runs go away with the next delete of them; the first deletion of the old skeleton may still need a second pass because their versions are old.

## [53] - 2026-11-23 — The log says why a deleted folder is kept

### Added
- **A folder that is kept after another node deleted it is now logged**, with what is in it ("kept the folder … it holds file … (in the index, live / not in the index / left out of the sync)"), and so is a change here that beats the other node's deletion (with both versions). The first twelve of each sync are shown, then how many more. Folders that come back as empty folders after a deletion could until now only be explained by guessing: the logs of v50 to v52 do not say which of these two it is. Both the sync log and the troubleshooting bundle carry the new lines.

### Verified
- gofmt, vet, `go test -race ./...`, the five cross builds. A new test reproduces a kept folder and checks that the log names what is in it. A new stress test (both nodes scanning and syncing in loops, a delete at random moments while the other node is still replicating) does not reproduce the returning folders, so this release does **not** claim to fix them; it makes the cause visible.

## [52] - 2026-11-23 — Emptying a large trash is immediate

### Fixed
- **"Empty trash" sat for 30 seconds with nothing happening** on a trash of 76,000 files. The node deleted every file while holding the folder's operation lock (so the sync waited too), and the dialog gave no sign that it was working. The trash is now moved aside in one step, so it is empty and the folder free at once, and the files are removed in the background; what a restart interrupts is removed when the folder is next opened. The button now shows "Working…" and cannot be pressed twice while any dialog action is running.

### Verified
- gofmt, vet, `go test -race ./...`, the five cross builds, `node --check`. Not checked on the cluster. Counting the files for the "Freed …" message still takes a moment on a very large trash.

## [51] - 2026-11-23 — A deleted folder that holds files the sync leaves out no longer comes back

### Fixed
- **A folder that was deleted came back as empty folders.** A node that receives a deletion removes the folder only when it is empty. Files that the sync leaves out (the folder's ignore patterns, such as `*.tmp`) are not in the index, so a deletion never took them away: the folder was "kept", its version moved past the deletion, and it was announced again, so the empty folders appeared on every node. A folder whose remaining content is only what the sync leaves out is now emptied (those files go to the trash) and removed. A file the sync does not leave out and that the other nodes do not know of still keeps its folder, as before. A new test reproduces the old behaviour (it fails without the change) and covers both cases.

### Verified
- gofmt, vet, `go test -race ./...`, the five cross builds. Not checked on the cluster: that `*.tmp` files are what held the folders on your nodes (a `find /var/lib/filedir/folders/root/projects -type f` on filedir2 would show them).

## [50] - 2026-11-23 — The upload footer says only how many, and Cancel

### Changed
- While files upload, the Files page footer says only "Uploading N of M…" and the Cancel button. It used to add a line for each big file going up (its path, how much was sent, "connection lost, trying again in 8 s"), which could be so long that it pushed Cancel off the screen, and then went away again. Those lines are gone. A failed upload still says which file and why when it ends.

## [49] - 2026-11-23 — The trash's names are checked on their own, not inside a longer condition

### Changed
- The check of a trash folder's name and of the names inside it is now a test of its own in front of each use (the trash listing, a restore, a removal and the path of a trash entry), where before it was one part of a longer condition. The meaning is the same: a name that is not local is refused, as before.

## [48] - 2026-11-23 — The same check for the trash's own names

### Changed
- The name of a trash folder (`when`) and the names inside it are checked with `filepath.IsLocal` where the trash listing, a restore and a removal make paths from them, and a restore no longer ignores the error of working out where a file goes back to. Nothing that was allowed is refused.

## [47] - 2026-11-23 — The name is checked where a path is made from it

### Changed
- The code scanner did not accept that a name is safe because `ValidName` said so, nor a check made on the finished path. The name itself is now checked with `filepath.IsLocal` at each place a path is made from one (the folder's paths, the trash, the trash listing), in front of the existing checks. A name that was refused before is refused now; nothing that was allowed is refused.

### Verified
- gofmt, vet, `go test -race ./...`, the five cross builds. Whether the scanner now closes its findings is only known when it scans again.

## [46] - 2026-11-23 — Deleted while it is being replicated: stop and delete; Cancel for uploads

### Changed
- **An item that is deleted while it is being replicated is dropped, not finished.** A sync takes as long as the folder is big, and a deletion made meanwhile used to wait for the next sync while the pull carried on, or came back on the node. Now a deletion made on this node (the Files page, MCP `files_delete`, WebDAV) and one that the peer announces during the sync (it is asked every 2 seconds) dooms the item, or the whole folder: the downloads under it stop at the next block, their temporary files are removed, the items under it that were not started are skipped, and the deletion is applied before the sync ends, so what had already arrived goes to the trash with it. The last step of a pull, the swap into place, is one step with respect to a delete, so a file is not put back into a folder that is being removed. The sync does not report an error for this.

- **A big deletion is applied much faster.** A folder deleted on another node arrives as one deletion for each file in it, and each used to be written to the index with a sync of its own, one after another: tens of thousands of syncs, which took minutes while the node showed *behind*. The deletions are now written a few hundred at a time, one sync for each, and the position in the peer's index is kept only after the last of them is in. A node that stops half way has taken files off the disk without recording it, and the next scan records the deletion, as for a file a person removed. How much faster it is was not measured on a cluster.

- **Two hardenings for the code scanner's findings.** A download link opened before signing in is rebuilt from its parts and its origin checked before the page goes on to it (it was already limited to `/api/files/download` and `/zip`), and the path of a name in a folder is checked again to lie inside the folder where it is made (`ValidName` already refused every way out). The other findings are left as they are, for reasons that are in the answer to the scan, not in the code: a fetch from a web address that is refused at connection time for private addresses, certificate checks that an administrator turns off or that are made by hand, and a key path an administrator sets.

### Added
- **Cancel** in the Files page footer while files are uploading: the files going up stop and the rest is not started; it says how many went up. Pieces of a big file that had gone up stay on the node until it clears them; nothing of it appears in the folder.

### Verified
- gofmt, vet, `go test -race ./...`, the five cross builds, `node --check`. Two new tests (a peer deletes a folder while it is pulled; the folder is deleted on this node while it is pulled) fail when the doom is switched off; a third has 700 files deleted on one node and applied on the other in batches. The live two-node check, and the Cancel button in a browser, were not made.

## [44] - 2026-11-23 — One deletion, one entry in the trash

### Fixed
- **A folder deleted on another node showed up in the trash as dozens of entries of the same name.** A deletion reaches the other nodes file by file, over many seconds, and each file was put into the trash folder stamped with its own second, so the trash listed `projects` once per second, each holding a slice of it. Deletions that arrive from other nodes now share one trash folder while they keep coming (a pause of a minute starts a new one), so the folder is one entry, and restoring or removing it is one step. A deletion made on the node itself still has its own stamp. Entries already in a trash stay as they are.

### Verified
- A test deletes five files of one folder seven seconds apart through the pull path: one entry holding all five, and a new entry after a two-minute pause. The live two-node check was not made.

## [43] - 2026-11-23 — Make folders, copy, move, rename, delete and the trash over MCP

### Added
- **`files_mkdir`, `files_copy`, `files_move` and `files_delete`**, MCP tools for read-write keys, so an assistant can do with the files of the synced folders what the Files page does (together with `files_list`, `files_read`, `files_write`, and the links of 42 for big files). `files_move` is also the rename. A copy or a move can go into another synced folder (`to_folder`), the folder it goes into must exist (`files_mkdir` makes it), and a name that is taken is refused unless `unique` is true. A delete, and a file that a copy or a move replaces, goes to the folder's trash on the node that took the call, as on the Files page: nothing is removed for good. A read-only key is refused all four.
- **`files_trash`, `files_trash_restore` and `files_trash_empty`**: look at the trash of a synced folder, put something back, and empty it. Emptying removes what is in the trash for good and needs `confirm: true`, so a call that leaves it out is refused. Each node keeps its own trash (a delete goes to the node that took it), so these work on the node that answers; call again to reach another.

### Changed
- The README, the help and the MCP server's own instructions said that MCP could not delete or rename; they now say what it can. Settings, the cluster, gateways, schedules, users and certificates still cannot be changed over MCP.

### Verified
- gofmt, vet, `go test -race ./...`, five linux builds. A new test makes a folder, copies (and the refusal of a taken name, and `unique`), moves, renames, moves a tree, refuses a move into itself, into a folder that is not there and of a file that is not there, deletes a tree and checks that nothing else went with it, and that a read-only key gets none of it.

### Not verified
- A live run on a real cluster, and what the other node shows after a delete (the same code as the Files page, but not looked at here).

## [42] - 2026-11-23 — Upload and download big files over MCP

### Added
- **`files_download_url` and `files_upload_url`**: two MCP tools that hand the assistant a short-lived link, so a big file is moved with plain HTTP (`curl -o file URL`, `curl -T file URL`) and never has to pass through the conversation as base64 text. Until now an upload over MCP meant typing the whole file out in parts of 700 KB, which is not workable past a few hundred KB (a 2.4 MB file is 3.3 MB of base64).
- A link names one file and a folder, is signed with a key made from the cluster secret, so any node accepts it, and stops working after the minutes asked for (15 by default, 1440 at most). A download link is the same kind as the public link on the Files page. An upload link has a key of its own, takes a PUT or a POST, makes the folders on the way and replaces a file that is there (the old one goes to the trash), as `files_write` does; only read-write callers can ask for one. A download link cannot write and an upload link cannot read.
- The links are served on the gateway addresses (next to `/mcp` and the files site) and on a node's own management port.

### Verified
- A new test makes both links through the MCP endpoint, uploads 3 MB by PUT, replaces the file by POST, downloads it, and checks that the links of the other kind, a changed token, an expired link and a link to a file that is not there are all refused and leave the file alone, and that a read-only key gets no upload link.

### Not verified
- Use from a real assistant on a real gateway. A link can only be used by something that can reach the gateway address: a chat in a browser, whose tools run in someone else's network, cannot, and still has the base64 parts. During a rolling upgrade, an upload link that reaches a node still on 41 is refused.

## [41] - 2026-11-23 — A page icon of its own

### Changed
- **The page icon (the one on the tab and the bookmark) is now a teal folder**, on the admin page and on the files page, so the six products can be told apart in a list of bookmarks. Until now filedir wore the same blue D as dnsdir. The other products got their own at the same time: dnsdir keeps the blue D, logdir is a green L, flowdir an orange F, netdir a purple N and ntpdir a red clock.

### Verified
- gofmt, vet, `go test -race ./...`, `go build` for linux amd64, arm64, arm, 386 and riscv64. The icon was looked at in headless Chromium only through the mock page.

## [40] - 2026-11-23 — README with pictures; Monitor columns

### Added
- `snaps/` holds four pictures of the GUI (the Files page, its right-click menu, Properties of a folder, and Monitor ▸ Files with the replication card). The README shows them. They were taken from the real page running against made-up data, so the names and numbers in them are examples.

### Changed
- The README is rewritten in plainer words, and two statements that were out of date are corrected: partly downloaded big files are shared between nodes now, and an MCP caller can read a file (and upload one with a read-write key).

### Fixed
- Monitor ▸ Files (from 39): the fixed column widths cut "Documents" to "D…" and hid the end of the Sync column. The browser sizes the columns again; they still line up from node to node because every node is in one table. The three replication tables also share column widths.

### Verified
- The browser tests for Files, the root folder, the files site and Monitor; vet, the race tests and the five builds.

### Not verified
- The pictures are of a mock, not of a live cluster.

## [39] - 2026-11-23 — Replication on the Monitor page; the filter looks where you are

### Added
- Monitor ▸ Files has a **Replication** card: for each folder, how every node stands with every other (in step, moving, behind or an error, and when it last asked), and the large files being pulled right now with how far each has come. Files under 8 blocks are over too quickly to show; they count in "moving N".

### Changed
- The filter in Files looks where you are: the open folder and everything below it. Only at the top of the root folder does it search every folder. The footer says where it looked. In the trash it narrows the trash listing by name (the level you are looking at), and nothing else.

### Fixed
- Monitor ▸ Files: the columns now line up from node to node (one table instead of one per node).

### Verified
- Tests for a search kept to a folder and a path; browser tests of the aligned columns (same right edge in both nodes), the replication matrix and a transfer bar, and the filter staying in the open folder.

### Not verified
- The replication card on a real cluster mid-transfer: only the mock shows a file on its way. "Moving" comes from what the last round took, not from live byte counts.
- The trash filter only looks at the listing already shown, not deeper levels.

## [38] - 2026-11-23 — Download as zip works in the management GUI

### Fixed
- "Download as zip" in the Files page of the management GUI gave 404: the route `/api/files/zip` was only registered on the files site. It is now on both. A test compares the file calls of the two route tables, so one that is added to only one of them fails the tests.

### Verified
- The new test fails with the route removed and passes with it; the full tests.

## [37] - 2026-11-23 — The filter box says "Filter"

### Changed
- The placeholder of the filter box is "Filter…" (it was "Filter all folders…"). What the filter searches did not change: every folder.

### Verified
- The three UI tests pass.

## [36] - 2026-11-23 — An interrupted upload takes up again

### Changed
- A file of 4 MiB or more goes up from the Files page in pieces of 8 MiB. When the connection drops, the page waits (1 s, then longer, up to 15 s between tries, for about ten minutes in all), asks the node how much of the file is there and sends the rest; the footer says "connection lost, trying again in 4 s" and shows how far each big file is. A reloaded page that uploads the same file again finds the part that is already there. The file goes in place only when all of it has arrived (`POST /api/files/upload/begin`, `/chunk`, `/finish`); what an abandoned upload leaves is removed after 48 hours.
- A small file whose upload fails on the network is sent again, up to five times.
- A full disk or a refused file is not retried.

### Verified
- A Go test: a first piece, a second `begin` that finds the same upload and where it ends, a piece at the wrong place refused with the right offset, another user refused, the rest, `finish` twice (a lost answer) and the file is identical; 40 of 100 bytes is not finished. A UI test cuts the connection on the second piece and the upload goes on and finishes (light and dark).
- Not verified: a real flaky link, a very large file, or a node restart half way.

### Known
- The pieces wait on the node that got them. If the next ask is sent by the gateway to another node, that node knows nothing of the upload and it starts again from the beginning.

## [35] - 2026-11-23 — Nodes help each other while a big file is still arriving

### Changed
- A big file (8 blocks or more) is shared while it is being pulled. A node pulling a file says which blocks of it it already has (`POST /cluster/files/have`, asked once a second) and serves them to the other nodes that are pulling the same version; the nodes take the blocks in a random order, so they have different ones to trade. Before, a node could only give blocks of a file once it was complete and in place, so the first copy of a file came from one uplink. A block is taken from the source with the fewest requests running that has it (up to 16 per source), the node that sent the entry is the last resort, and every block is still checked against its hash: a node that lies about what it has, or sends wrong bytes, only costs time. A node without the new endpoint is given up on after three failed asks.
- Properties of a file and of a folder have the same lines in the same order: what it is, Size, Folder, Path, On disk, Modified, Last changed by. A folder's size is everything under it.

### Verified
- Tests: two nodes pulling from a slow origin gave each other blocks (the origin served 218 of 320 blocks in the test); a node that says it has everything and gives nothing, or gives wrong bytes, does not stop the file arriving; a download under way serves exactly the blocks that arrived; the endpoint over the cluster channel; all with `-race`. The transfer rate between two nodes on one machine is unchanged (2.2 to 2.4 Gbit/s).
- Not verified: real networks. Over a fast local link the transfers end before the nodes can help each other, so the gain shows where the uplink of the node that has the file is the limit. The saving in the test (about a third with three nodes) is not a measure of what a cluster will see.

## [34] - 2026-11-23 — Dialog buttons on the right

### Changed
- The buttons of every dialog sit on its right side. In the confirm and ask dialogs the main button is the right-most one, with Cancel to its left.

### Verified
- The Files UI test checks that the buttons sit at the right edge; the three UI tests pass. Not checked one by one: every other dialog in the app (they share the same rule, but a dialog with its own layout may differ).

## [33] - 2026-11-23 — Folder size in Properties, for the root folder too

### Fixed
- Properties of a folder inside the root folder, and of a synced folder shown in the root, gave no size: the page skipped the lookup for the root folder. It now asks for them like any other (a synced folder shown in the root gives the size of the whole folder).

### Verified
- The Files UI test opens Properties of a folder and sees the size and the counts; the size endpoint test from [32]; vet and the tests of the web package.
- Not verified: against your cluster.

## [32] - 2026-11-23 — Moves are instant; sizes in Properties; errors under the footer

### Fixed
- Dragging a file or folder to another place made it vanish and come back minutes later: the rename dropped the old names from the index and left the new ones to the next scan, which read and hashed all of it again. A rename now moves the index entries to the new names at once (size and time do not change in a rename); the scan finds nothing to do. An entry that does not match the disk is still left to the scan.

### Changed
- Red error messages on the Files page are shown below the footer line, inside the card, not above the page.
- The folder menu in the tree has Properties where it had Refresh.
- Properties of a folder says how big everything under it is, all the way down, with the counts of files and folders (`GET /api/files/size`, from the index). Properties of a file already gave its size in bytes.

### Verified
- Tests: a rename (file, folder tree) is in the index at once, the scan finds nothing left, a peer ends with the same tree; the size endpoint adds up three levels; the UI tests for the message position and the menu.
- Not verified: a move of a multi-GB folder on your cluster. A move between two different synced folders is still a copy and delete (with progress and Cancel); only a move inside one synced folder is instant.

## [31] - 2026-11-23 — A copied file shows up when the copy ends

### Fixed
- After a copy or duplicate had finished writing, the node read the whole file again to hash it before it showed in the list and could start to reach the other nodes: minutes of nothing for a big file. A copy now hashes the file while it writes it and puts it in the index at once (`WriteFileHashed`); the next scan finds nothing to do. This also applies to files written through WebDAV copy.

### Verified
- A test that the entry equals what a scan records (the scan then finds no change), that a peer receives the same content, and that rewriting with other content makes a newer version; the full tests.
- Not verified: the time saved on a multi-GB file on a real disk. Reaching the other nodes still takes the time the network needs, and the footer does not show that yet.

## [30] - 2026-11-23 — A copy can be cancelled

### Changed
- The footer's copy progress has a Cancel button. Cancelling stops the copy, removes what was copied so far and leaves the source as it was; for several items, the rest are not started. `POST /api/files/copy/cancel`.

### Verified
- Tests: a cancelled copy of 400 files keeps nothing and the source is whole; another user cannot cancel it; the UI test sees the button.
- Not verified: cancelling a copy of a very large single file on a real disk (it stops at the next read and the temporary file is removed).

## [29] - 2026-11-23 — Copying shows how far it is

### Changed
- Copy, Duplicate, Paste and moves between folders now run in the background on the node. The footer says at once what is happening and keeps counting ("Copying big.iso: 3.2 GB of 20.5 GB (15%)") until it is done; before, nothing showed until the copy had finished, minutes later for a big file. `POST /api/files/copy` takes `async`, and `GET /api/files/copy?id=` says how far it is.

### Verified
- Tests: a background copy and move report progress and end with the right content, another user cannot see it; the Files UI test sees the footer's progress.
- Not verified: how long a copy of a very large file takes on your disks. This shows progress; it does not make the copy itself faster.

## [28] - 2026-11-23 — Files-site sessions of directory and SSO users follow them

### Fixed
- A person who signed in to the files site through the directory (LDAP) or single sign-on was signed out when the next request reached another node. The session travelled between nodes without the mark of how it was made, so the other node checked the person's group as a local (PAM) account, found none and ended the session. The marks now travel with the session.

### Changed
- The Files footer says why a node is not syncing in a few words (for example "no route to host, timed out") instead of every dial error; the full text is its tooltip.

### Verified
- A test that the marks survive the hand-over; the session tests pass.
- Not verified: on a real cluster with a directory or single sign-on provider. A person with a local account that is missing on some node would still be signed out there (the README says the account must exist on every node).

## [27] - 2026-11-23 — The top folder stays closed

### Fixed
- Closing the top folder in the Files tree opened it again at once: every refresh re-opened the root and the current folder. Each is now opened once, the first time it is seen.

### Verified
- The Files and root UI tests pass. Not verified: a dedicated test for collapsing the top folder.

## [26] - 2026-11-23 — Many small files, faster

### Changed
- Many small files sync much faster. Three changes: a sync pulls up to 12 files at once instead of one at a time; the index journal and the directory syncs are shared between writers that arrive together (group commit; the order data → rename → directory → index is unchanged); and the small single-block files of a sync are fetched in batches (`/cluster/files/blocks`, up to 256 blocks per request) instead of one signed request each. A peer without the new endpoint, or a batch that fails or does not match its hashes, falls back to the old per-block request.

### Verified
- `go test -race` for all packages, with new tests for batching, batch failures (error, missing, corrupt), a cut-off half way, conflicts pulled in parallel, and concurrent journal writes.
- On one machine (loopback, fast disk), 2000 small files between two nodes: 437 files/s before, 1343 files/s after (`TestSmallFilesTransferRate`), about 3×.

### Not verified
- Real networks and slow disks: the gain should be larger where requests wait on latency or fsync is slow, but it was not measured. No real power-loss test; the ordering rules are covered by the existing crash-stage tests.

## [25] - 2026-11-23 — Nodes are named, not numbered

### Changed
- **Nodes are shown by host name** (filedir1), not by id (6cb0852d93696194): the Node column of the file list and of matches, "Last changed by" in Properties, the node headings of the Files status page, and "Not syncing with …" in the footer. A node that has left the cluster keeps its id.

### Verified
- gofmt, vet, `go test -race ./...`, `CGO_ENABLED=0` vet and tests, builds for linux amd64, arm64, arm, 386 and riscv64; a new test for the names.

### Not verified
- Not checked against a real two-node cluster: the test covers this node's own name and the fallback to the id.

## [24] - 2026-11-23 — The trash node is called "trash"

### Changed
- The node at the end of the folder pane is labeled "trash", not ".trash" (the folder on disk is still `.filedir/trash`).

### Verified
- gofmt, vet, `go test -race ./...`, `CGO_ENABLED=0` vet and tests, builds for linux amd64, arm64, arm, 386 and riscv64; the file manager UI tests.

## [23] - 2026-11-23 — A full disk gives up the trash before it refuses files

### Fixed
- **A node whose disk was near the limit stopped taking files that the other node had.** In the bundle of filedir2: a 32 GB disk at 77 % (the cap is 80 %), and filedir1 had just fetched a 10.8 GB ISO, so filedir2 refused to pull it, and nothing said so. The trash of a folder (a copy kept for days) held part of that space. Now, before a write or a pull is refused for lack of room, the folder's trash is emptied (except what was put there in the last 10 seconds) and the disk is measured again.
- **Why a node is not syncing is now said.** The Files footer adds "Not syncing with <node>: <reason>", and the log gets a warning when syncing with a peer starts to fail or the reason changes (and a line when it works again). Before, a refused pull showed nowhere.

### Verified
- gofmt, vet, `go test -race ./...`, `CGO_ENABLED=0` vet and tests, builds for linux amd64, arm64, arm, 386 and riscv64; a new test: the old trash is emptied before the refusal, a fresh one is kept.

### Not verified
- The bundle holds no file status or pull error, so that the refusal was for room is inferred (77 % used, a 10.8 GB file, an 80 % cap), not read. If the nodes still do not sync, the new footer line and log warning will say why. A disk too small for the file even with an empty trash still refuses: raise the cap (Configure ▸ Folders) or free space.

## [22] - 2026-11-23 — A big folder can be deleted, and the trash is a folder

### Changed
- **The trash is a folder in the tree.** The ".trash" node at the very end of the folder pane (management GUI) shows the trash of the open folder on this node like any folder: deleted files and folders, newest first, with when each was deleted and its size; a folder opens (".." goes back). Right-click: **Restore** (a folder goes back file by file to where it was; a name that is taken again becomes "name restored", nothing is replaced), **Delete…** (for good), **Empty trash…**. This replaces the "View trash…" dialog of v21. API: `GET /api/files/trash/list` (one level: `when`, `path`), `POST /api/files/trash/restore`, `POST /api/files/trash/delete`.
- **The file menu is in a set order:** Open, Download | Copy, Rename, Delete | Properties.
- **The menu of a folder in the tree is in a set order too:** Open, Download | Upload, New | Rename, Delete | Refresh. "Copy path" and "Empty trash…" left it (the path is under Copy ▸ Path of the file menu, the trash is the .trash node).

### Fixed
- **A folder with hundreds of files in it could not be deleted.** Deleting moved it to the trash, but the scan that was to record the deletions refused (it will not delete most of a folder on its own: a disk that came back empty must not wipe the other nodes), nothing was recorded, and the folder and its files stayed in the list on every node. A delete or a rename made by a person now records the deletions at once, and the guard stays for what the scan finds on its own. A folder that an earlier version left like that (gone from the disk, still listed) is cleared by deleting it again.

### Verified
- gofmt, vet, `go test -race ./...`, `CGO_ENABLED=0` vet and tests, builds for linux amd64, arm64, arm, 386 and riscv64; the file manager UI tests (the .trash node: list, open a folder, "..", restore, delete, empty; the menu order); the trash backend (list by level, restore of files and folders, delete, paths outside the trash refused); a new test: 300 files deleted or renamed in one go, the state an older version left, and the scan guard still refusing a disk that came back empty.
- Found in the troubleshooting bundle of filedir1: "deleted test (kept in the trash)" at 05:45:08, then no sign of the deletion reaching the index.

### Not verified
- Not run on the user's cluster. The bundle shows the delete itself succeeded and the folder (over a thousand files, most of the folder) is just the size the guard refuses; the refusal itself is not in the log (the scan's error shows only as "Problem" in the Files footer), which is why the cause is inferred.

## [21] - 2026-11-23 — A full disk empties the trash, and the trash can be looked at

### Added
- **View trash…** (right-click ▸ in the Files page on the management GUI, beside Empty trash…): lists what is in this node's trash of the folder, newest first, with when it was deleted and its size, and a **Restore** on each file. A file goes back to its own path; a name that is taken again gets "name restored". Restoring is a rename inside the folder, so it needs no room, and the next scan shares it with the other nodes. API: `GET /api/files/trash/list`, `POST /api/files/trash/restore`.
- **While a "Disk nearly full" alert is open on a node, the node empties the trash of every synced folder on that disk** (each round of the alert checks, so what is deleted while the disk stays full goes too). A folder on another disk, or with an empty trash, is left alone. Each emptying is logged as a warning with the folder, the files and the bytes. The trash is per node, so only the node with the alert gives it up. It follows the alert as configured: if the rule is switched off, or its threshold (90 %) and wait (10 minutes) are not reached, nothing is emptied.

### Changed
- **Replication between nodes is faster.** A block is one request to the peer, and a file was fetched with 4 requests in flight from each peer, in 128 KB blocks, so a fast link was left waiting on round trips (about 2 Gbit/s). Now 16 requests are in flight per peer (64 at most), files of 32 MiB and more are cut in blocks of at least 1 MiB, and a fetched block is read into one buffer of its known size instead of being grown and copied. Nodes with different block sizes still work together (a block is reused only where its hash matches).

### Fixed
- **A folder with no folders in it showed an arrow in the tree.** The listing now says whether a folder has folders in it (`has_dirs`), and the tree shows the arrow only for those.
- v20 was packaged with one directory level too many, and Upload a Release refused it ("source/go.mod … not found"). The archive has `filedir/source/…` again.

### Verified
- gofmt, vet, `go test -race ./...`, `CGO_ENABLED=0` vet and tests, builds for linux amd64, arm64, arm, 386 and riscv64; the file manager UI test (view trash, restore); trash list and restore (new test, including names outside the trash); and a new test: no alert leaves the trash, an open alert empties it (again for later deletions), a disk without a folder of ours is left alone.

### Not verified
- The replication change was measured only between two nodes on one 2-core machine (128 MiB: 2.16 Gbit/s before, about 2.6 after), where both nodes and the encryption share the same two cores; on two machines the gain is not known. Files are still fetched one after another and each is flushed to disk before it is swapped in, which limits many small files and slow disks.
- Not run against a really full disk, and not through the real host statistics that raise the alert: the test feeds the alert engine a "full" sample for the folder's disk.

## [20] - 2026-11-23 — Faster file operations, and rename in the list

### Changed
- **Renaming is inline.** A slow second click on the selected item (or Rename in the menu) turns its name into a text box in the row, with the name without its extension selected. Enter or clicking elsewhere renames, Esc keeps the name. A failed rename is said in the footer.

### Fixed
- **File operations took seconds on a large folder.** Every listing, status call and tree node copied and sorted the whole file index, and each action reloads the status, the list and every open tree folder. The sorted index is now built once and kept until it changes (100,000 entries: 60 ms for the first, under a microsecond after), and the reload asks the list and the tree at the same time, not one after the other.

### Added
- A request of the files site that takes over a second on this node is logged as a warning with the call and the time, so a slow site can be told from a slow network.

### Verified
- gofmt, vet, `go test -race ./...`, `CGO_ENABLED=0` vet and tests, builds for linux amd64, arm64, arm, 386 and riscv64; the snapshot cache and its invalidation (new test); the file manager UI test (inline rename: no dialog, extension left out of the selection, Esc keeps, Enter renames).

### Not verified
- The delay was not seen on the user's cluster: the cache fixes the cost that was measured here, but another cause (a slow network, a slow disk, LDAP or SSO rechecks) would still show. If it is still slow, the new warning lines in the log say which call and how long.

## [19] - 2026-11-23 — Bandwidth limits

### Added
- **Upload limit**, **Download limit** and **Replication limit** (Configure ▸ Folders, shared by the cluster, Mbit/s, 0 or empty for none, none by default). Each is per node and shared by every transfer of its kind on that node. Uploads: the Files page, WebDAV writes and fetch from URL. Downloads: the Files page, zips, public links and WebDAV reads. Replication: what the node sends to the other nodes (so three nodes at 100 can send 300 between them). A change applies to transfers that are already running. A node that is held to a few Mbit/s is given more time per block by the node that asks. No schedule.

## [18] - 2026-11-23 — File replication is faster between nodes

### Changed
- **Replication was held back at about 1 Gbit/s by design, not by a setting** (there is no bandwidth limit anywhere). Every block of a file (128 KB for a file up to 256 MB) was fetched on a new connection with a full TLS handshake, and with only one source at most two blocks were in flight, so the time went to handshakes and waiting, not to the data. The file transfer port (4452) now keeps its connections between requests (a request that finds a kept connection dead is sent again on a new one), and up to 4 blocks per source (16 in all, was 2 and 8) are fetched at once. On one machine, between two nodes, a 128 MiB file went from 0.89 to about 2 Gbit/s; across a real network, where each handshake also cost round trips, the gain should be larger. Control calls between nodes are unchanged.

## [17] - 2026-11-23 — Empty the trash

### Added
- **Empty trash…** in the Files page of the management GUI (right-click the empty space, or a synced folder in the tree): it says how many files and bytes the folder's trash holds on this node, and on confirming deletes them for good. Deleted and replaced files were kept 30 days with no way to free the space sooner. The trash is each node's own, so the other nodes keep theirs; the files site does not offer it. `GET /api/files/trash` and `POST /api/files/trash/empty`.

## [16] - 2026-11-23 — Slow double-click renames

### Added
- On the Files page, a **slow second click** on the one selected file or folder (more than about a third of a second after the first) opens **Rename**, as in a file manager. A quick double-click still opens it.

## [15] - 2026-11-23 — WebDAV, and a limit on how full the disk may get

### Added
- **Copy, paste, duplicate and drag between folders** on the Files page. **Copy ▸ To paste** holds the file, folder or selection; **Paste** (on a folder, or the empty space) puts it there, also into another synced folder, with "name copy" for a name that is taken. **Copy ▸ Duplicate** makes "name copy" beside it. Drag files and folders onto a folder in the list or the tree to move them (hold Ctrl or Cmd to copy). Copy and Path (and for files the link and public link) are in the same Copy menu. The server does it in one call, `POST /api/files/copy`.
- **WebDAV on the files site**, at `https://<files address>/dav/`. Map it as a network drive in Windows ("Add a network location"), connect to it in Finder (Go ▸ Connect to Server), or use davfs2, rclone, Cyberduck or Office. It is the same folders, people and rules as the Files page: the name and password are the sign-in page's (host accounts, then the directory) with the same lockout; **Full-access** and **Files-only** groups may change files, the **read-only** groups may only look. The layout is the Files page's: with a root folder (`/`), `/dav/` is that folder with the other folders inside it, otherwise `/dav/` lists the folders. Supports listing, download (with ranges), upload, folders, rename/move, copy, delete (to the folder's trash) and locks (handed out, not enforced). A person who signs in through SSO has no password, so cannot use it. Writes are made in this node's copy and reach the other nodes at the next scan, as for the Files page.
- **Stop at disk usage** (Configure ▸ Folders, shared by the cluster, **80%** by default, 100 turns it off): nothing is written or fetched into a folder that would take its disk past that percent full, so the files cannot fill the host. It covers uploads, fetch from URL, WebDAV, files arriving from other nodes (retried when there is room) and the chunked MCP upload, and is measured the way `df` does, on each node's own disk. A refusal says so (HTTP 507) and a long upload stops where the limit is.

## [14] - 2026-11-23 — Upload from URL shows how far it is

### Changed
- **Properties** (management GUI only) also shows **On disk (this node)**: the real path of the file or folder on the host, so a synced folder's files can be found on the filesystem. A test now checks that a write to the root folder lands at its configured path.
- **Upload from URL** now runs in the background: the dialog closes as soon as the server has answered, and the footer under the list says how much has come ("Fetching name: 12.3 MB of 40.1 MB (31%)", or just the amount when the server does not say the size), with **Cancel**. The list is refreshed when the file is in. A failure is shown in the page; a cancelled or failed fetch leaves no file. Refusals that can be known at once (an address that is refused, a server that says no, a bad name) are still answered at once.
- This node runs at most 4 fetches at a time. A fetch is the node's and the person's who started it: it is asked about on the same node, so behind an address that spreads connections over several nodes the status can be asked of another node, which says it does not know it (the file still arrives).

- **The right-click menus are shorter.** A file's **Copy** is one submenu (Link location, Public link, Path); the empty space and a folder have **Upload** (Files, Folder, From URL) and **New** (Folder, File); **Refresh** and **Up one folder** are gone from the empty-space menu (the list refreshes by itself, and the tree and the folder's own entry go up).

### Verified
- Go tests (race, CGO off): the byte count and total midway, completion, cancel (nothing kept) and that another user cannot see a fetch; the footer progress and the Cancel button in headless Chromium.

## [13] - 2026-11-23 — One list of groups

### Changed
- **One list of groups instead of three.** Configure ▸ Web GUI has **Full-access groups**, **Read-only groups**, and for people who may use the Files page only, **Files-only groups** and **Files-only read-only**. The same names are looked for on the host and, when LDAP is set up, in the directory, and the full-access and read-only groups open the management GUI and the files site at the same level. The LDAP tab no longer has groups of its own (servers, bind account and search base only); a configuration that still has `ldap_rw_group` or `ldap_ro_group` keeps using them for the management GUI.
- The files-only groups are an addition to the others, not a replacement: before, setting Files site groups took the files site away from the login groups. A person can have read-write access to the files site and none to the management GUI by being in a files-only group alone.
- A group name that the directory does not have is skipped (and said in the LDAP Test box) instead of failing the check, since the list is shared with the host's names.
- Renamed: Login groups ▸ Full-access groups, Files site groups ▸ Files-only groups, Files site read-only ▸ Files-only read-only. The setting names in the configuration file are unchanged.

### Verified
- Go tests (race, CGO off): the directory takes its groups from the common list (a missing name skipped), a files-only user signs in to the files site and not the management GUI, a full-access user signs in to both, LDAP and SSO on the files site as before.

### Not verified
- A real directory; and a DN mixed with plain names in one list (a value that has an `=` in it is read as one DN, so give a DN alone).

## [12] - 2026-11-23 — Directory and single sign-on users on the files site

### Added
- **The files site takes directory (LDAP) and single sign-on users**, the same people at the same level as on the management GUI (full access or read-only by their directory group or their place in the list of users). Their sessions are shared among the nodes like any files-site session and are rechecked the same way as on the GUI. The login page shows the single sign-on buttons.
- For single sign-on the files site returns to its own host, so `https://<gateway name or address>/api/sso/callback` has to be registered at the provider (the help says so). A sign-in has to start and end on the same node.

### Not changed
- There is still no separate directory group for files users alone: the LDAP full-access and read-only groups, and the list of SSO users, are the management GUI's and now let people into the files site as well. Host (PAM) accounts can still be limited to the files site with **Files site groups**.

### Verified
- Go tests (race, CGO off): an LDAP user and an SSO user sign in to the files site and see files; a wrong password and an SSO user not on the list do not; a files session is not accepted by the management GUI.

### Not verified
- A real directory server or provider, and an SSO sign-in that starts and ends on different nodes behind an anycast address.

## [11] - 2026-11-23 — The files site is always on; public links; upload from URL

### Changed
- **The files site is always on.** It answered 404 to everything until **Files site groups** was set, so a copied download link was a bare "404 page not found" on a fresh setup. With no files group set, the people who may sign in to the management GUI (its login and read-only groups) may sign in to the files site; setting **Files site groups** gives it to other people instead. The site is always there; with no group at all nobody can sign in.
- A download link opened in a browser that is not signed in goes to the sign-in page and then straight to the download, instead of showing `not logged in`. A script or curl still gets the 401.

### Added
- **Copy public link…** (right-click a file; accounts that may change files): a link that downloads that one file with no sign-in for 1 to 30 days. It is signed with a key derived from the cluster secret, so every node and the anycast address accept it and nothing is stored; it cannot be taken back before it expires. Making one and every use of one are logged.
- **Upload from URL…** (the menu of the empty space and of a folder, after Upload folder): paste a web address and the node fetches the file into the folder (http and https; the name comes from the server or the address). It never connects to loopback, link-local, multicast or unspecified addresses, checked where the connection is made, so a redirect or a name that resolves there is refused too; private network addresses are allowed.

### Verified
- Go tests (race, CGO off) for the fallback groups, the sign-in redirect, public links (valid, changed, expired, out of range, read-only account) and the fetch (redirect, file name, refusals, read-only account); the Files page in headless Chromium.

### Not verified
- A fetch from a real server over TLS, and a public link through the anycast address on a real cluster.

## [10] - 2026-11-23 — Reverse DNS for the gateway names

### Added
- DNS registration now also registers a PTR record for every address of a gateway name: the shared addresses (and more addresses) of a gateway point at its VIP FQDN, its anycast addresses at its Anycast FQDN. An address paused on every node gets none; an address already given a PTR as a node's own keeps that. Like the names themselves, these are kept right by every node with registration on and are not removed by Remove records.

### Verified
- Go tests (race, CGO off), including the PTR of every address against the fake DNS server with TSIG and that a second run sends no update.

### Not verified
- A real DNS server and reverse zone: it depends on the reverse zone accepting dynamic updates under the same key (the same as the node's own PTRs).

## [9] - 2026-11-23 — The DNS page says the gateway names are registered

### Fixed
- **Operate ▸ DNS:** the note under DNS registration still said the shared addresses of the gateways are never registered, which read as if a gateway's VIP FQDN and Anycast FQDN were left out. They are registered (the VIP FQDN with the gateway's shared addresses, the Anycast FQDN with its anycast addresses, on every node that has registration turned on); the note and the README now say so.

### Not verified
- The records on a real DNS server for the reporting cluster: its last check showed 10 records right, which fits the node's 6 plus 4 for the two names, but the names were not listed in the bundle seen.

## [8] - 2026-11-23 — A failed Topology save no longer stops the saves after it

### Fixed
- **Topology page:** saves are queued, and one that threw an error (rather than being refused with a reason) left the queue rejected for good: every later change on that page, such as setting a gateway's VIP FQDN and Anycast FQDN, was then dropped without a request ever reaching the daemon, until the page was reloaded. The failure is now shown ("Not saved: …") and the queue goes on. This matches what a troubleshooting bundle showed: no save of the configuration reached either node after the v5 update, though the daemon accepts and shares the names (replayed with the real configuration: one node, and a primary and a replica).

### Verified
- Go tests (race, CGO off); the Edit gateway dialog in headless Chromium sends both names; the refused-save message still shows.

### Not verified
- That this was the cause on the reporting cluster: no request was seen, so the page is the suspect, but the failure itself was not captured. If it happens again, the message now says why, and the browser's developer tools (Network tab, on Save) show whether a PUT /api/config is sent.

## [7] - 2026-11-23 — A refused save on Topology is said, not undone; names refused while an old node is in the cluster

### Fixed
- **Topology page:** a save that the daemon refused (an error, a settings change made elsewhere) silently put the page back to what was saved, so what had just been typed, such as the VIP FQDN and Anycast FQDN of a gateway, vanished with no word. The reason is now shown at the top of the page ("Not saved: …").
- A change to a gateway's VIP FQDN or Anycast FQDN is refused, naming the node, while a node of the cluster runs a version before 3: such a node drops the names when the settings are shared, so they would disappear from every node a moment after being saved. Update the node first (Operate ▸ Upgrade).

### Verified
- Go tests (race and CGO off) including the refusal rules; the Topology page against a refusing stand-in (the message shows), and the Files page, files site and root view in headless Chromium.

### Not verified
- A cluster with a node on an older version (the check is tested on the rules, not on a live cluster). Whether the names survive a restart on a real cluster: they do on one node (the tests save, reload the file and read them back).

## [6] - 2026-11-23 — Files tree: going up closes what was below; open icons all the way up

### Changed
- Going to a folder in the tree (clicking it, opening it from the list, Up) closes the folders that were open below it, so the tree does not keep the way it came down.
- The open-folder icon is shown on the open folder and on every folder above it, the root included while a folder in it is open.

### Verified
- Go tests (race and CGO off), vet, gofmt, cross-compiles; the Files page, the files site and the root view in headless Chromium.

## [5] - 2026-11-23 — A root folder; FQDN fields named VIP FQDN and Anycast FQDN

### Added
- **A root folder.** A synced folder named `/` (any path) is the root of the Files page: the other synced folders are listed inside it, and hang under it in the tree, wherever each is kept on the disk; the root's own files and folders are listed beside them. A real folder in the root with the name of a synced folder is hidden by it. Opening a synced folder from the root, a drop on it and "Up to /" work; rename and delete are not offered on them from inside the root. Without a `/` folder the page is as before. The root's index is kept under `files/_root` (a name no folder can have). The root's zip holds its own files only.
- Configure ▸ Gateways has the two name fields, so a save from there no longer drops them.

### Changed
- The Files page: the filter box is centered in the frame; the tree and the list take the height of the window (they scroll inside); the page works whichever node the Node menu has picked (it used to show "this part of the GUI always acts on the node you are logged in to"): the files are the same on every node, and the page always acts on the node you are signed in to.
- The tree marks the open folder with an open-folder icon. Clicking a match in the filtered list opens the way to its folder in the tree and marks it (a folder match marks itself), and the matches stay as they are.
- The gateway name fields are called **VIP FQDN** and **Anycast FQDN** (settings `fqdn` and `anycast_fqdn` as before).

### Verified
- Go tests (root name rules, root listing with mounts and a hidden real folder, search) and the Files page in headless Chromium with a root. A new gateway's two names were followed from the page's save, through the config endpoint and on the replica-to-primary path in tests, and arrive each time.

### Not verified
- The report that names given while creating a gateway were not saved could not be reproduced in tests or in the browser; if it happens again on a real cluster, the daemon's log line for that save and the version of every node would help (a node on version 2 or older drops these fields when it re-shares the settings).
- A root on a real cluster.

## [4] - 2026-11-22 — Filter box sits beside the Files title

### Changed
- The Files page's filter box is half as wide and on the same line as the title (it had filled the whole header and wrapped under it).

### Verified
- The Files page in headless Chromium: the box is on the title's line at about half the header's width; the earlier checks still pass. The Go tests were run for v3 and this changes only a stylesheet rule.

## [3] - 2026-11-22 — Download and upload files over MCP

### Added
- **MCP `files_read` and `files_write`.** Download a file (a text file whole as text, else base64 in parts of up to 512 KB with `offset`/`length`) and upload one (read-write keys only; up to about 700 KB a call, larger files in parts that appear in the folder only when the last arrives). They use the same atomic write, trash and instant sync as the page; the call is logged and counted as a change in the MCP usage page. The privacy level still applies to text content (use base64 for exact bytes).

### Changed
- The MCP help, the initialize text and the refusal text no longer say that MCP only reads: `files_write` changes files, and nothing else can be changed.

### Verified
- `go test -race ./...`, `CGO_ENABLED=0 go test ./...`, vet, `gofmt`, cross-compiles for linux/amd64, arm64, arm, 386, riscv64. New tests: text and binary round trips, reads in parts, writes in parts (nothing visible until the last, wrong offset refused, restart discards), refused paths (`..`, `.filedir`, absolute), unknown folder, bad base64, a folder read as a file, a read-only caller neither sees nor can call `files_write`.

### Not verified
- A real MCP client (only the JSON-RPC endpoint was driven by the tests).
- Sending a file larger than a few MB in parts over a real network, and the stale-stage cleanup after an hour.

## [2] - 2026-11-22 — Files page rebuilt, folder download, instant change detection, files site for end users

### Added
- **Files site for end users.** Every gateway address (VIPs, further addresses, anycast) now serves, on the MCP port (443) beside `/mcp`, a web site with a sign-in page and the Files page and nothing else (`webfiles.go`). Every other path is a 404, so no setting, node or address is reachable through it. It is off until `web.files_group` (full access) or `web.files_read_group` (look and download) names a group: two new fields under Configure ▸ Web GUI, shared by the cluster (`SharedConfig.web_files`, omitted while empty so the cluster hash of a cluster without it is unchanged). Sign-in is PAM plus the group; there is no directory or single-sign-on login there. Its sessions have their own cookie (`__Host-filedir_files`) and are refused by the management GUI, and the GUI's by the site. End users are not shown the host, the configuration path, where a folder is on disk, node ids or sync state. The management GUI still has the Files page and still refuses connections that arrive on a gateway address.
- **Shared sessions** (`websessshare.go`, `/cluster/websession` on the cluster port). A files-site session is sent to the other nodes when it is made, when its access changes, as it is used (at most every 20 s, so the idle timeout holds everywhere) and when it ends. A node that meets a cookie it does not know asks the others (limited to 20 a second, cookies not shaped like a token are not asked about, and a miss is remembered for 30 s). Records carry the hash of the cookie, never the cookie, and are checked like input from a stranger. A signed-out session is remembered as ended so a late message does not revive it.
- **Copy link location** in a file's right-click menu copies the download address: the anycast name (or address) first, else the gateway's name (or address; IPv4 before IPv6), with the port when it is not 443. Two new gateway settings, **VIP FQDN** (`fqdn`) and **Anycast FQDN** (`anycast_fqdn`), are shared by the cluster and omitted while empty (the cluster hash of a cluster without them is unchanged). DNS registration (Configure ▸ Settings ▸ DNS) also registers both names when they are set: the gateway's name points at its shared addresses, the anycast name at its anycast addresses (A and AAAA, an address paused on every node is left out). Every node keeps them right, and `--dnsreg-remove` leaves them alone because they belong to the cluster. The link needs a sign-in on the files site.
- **Folder download.** `GET /api/files/zip?folder=&path=&path=…` streams a zip of files and folders as it is read (`filesync.WriteZip`): no memory or disk is used for the archive, links, the metadata folder and ignored names are left out, names that could be read as paths on another system are skipped, two items with one name are told apart by a number. The read and write time limits are lifted for downloads and uploads, so a large transfer is no longer cut off after a minute or five.
- **Filter box** in the Files header: finds files and folders in every synced folder and subfolder at once (`GET /api/files/search?q=`, up to 500 matches, every word must be in the path, case ignored; on the files site too). Matches show their folder; the menu offers Show in folder, open, download, copy link and properties, but not rename or delete (do that in the folder). Esc or opening a folder clears it.
- **Multi-select.** Click, Ctrl/Cmd+click, Shift+click (Ctrl+Shift adds a range), Ctrl+A, arrow keys with Shift, Home/End, Esc, Delete. The right-click menu acts on the selection: zip download, delete, copy paths, properties.
- **Drag in files and folders** from the computer onto the list, a folder in the list or a folder in the tree, with their structure (empty folders are made); **Upload folder** in the menu does the same from a chooser. Three uploads run at a time and the footer says how far they are.
- **Immediate change detection** (`fd_watch.go`). inotify watches every directory of a synced folder: a change is scanned about 0.4 s after it stops (never later than 3 s after it began), and a scan that found changes sends `/cluster/files/notify` so the other nodes pull at once. The 30 s scan stays as the safety net, and is the only one when inotify is unavailable or the watch limit is reached (logged once).

### Changed
- **Files page.** The folder tree and the file list are the same height; the files site's header has the user, theme and Sign out at the right. The toolbar buttons (Upload, New folder, Refresh) and the breadcrumb bar are gone; everything is in the right-click menu of a file, a folder, a tree folder or the empty space (there is always some below the list). The folder drop-down is replaced by a folder tree on the left (lazy, keeps its open state, right-click and drop work on it). Single files download as a link the browser follows, not through memory.
- The unused and invisible "Uploading…" and "Copied" status lines of the Files page were removed (progress is in the footer).
- A synced folder that failed to open no longer panics when it is closed.

### Verified
- `gofmt -l .` clean; `go vet ./...`; `go test -race -count=1 ./...` (new: zip, watcher, notify between two real in-process cluster nodes, files-site sign-in/isolation/read-only/CSRF/404 list, session sharing in process and over the real cluster channel, hostile records, shared groups); cross-compiles for linux/amd64, arm64, arm, 386, riscv64; `CGO_ENABLED=0 go vet` and `go test`.
- The Files page, driven in headless Chromium (light and dark) against a stand-in API: tree, selection with mouse and keyboard, menus for one and many, delete of many, zip download, folder picker, drops of files and nested folders (with batched `readEntries`) on the list, a folder row and a tree folder, the files-site page (no navigation, no Node column, read-only menus).

### Not verified
- A drag from a real desktop (Chromium's own `webkitGetAsEntry` was replaced by a stand-in in the test), and Safari/Firefox.
- Real PAM sign-in on the files site, and a live run of two daemons with a VIP moving between them while a person is signed in; the unit tests cover the same paths with fakes and with two real in-process cluster nodes. IPv6 binds.
- A person signed out on one node while another was down is still signed in on the one that was down until it expires (the memory of ended sessions is not kept over a restart).
- End-user accounts are system or directory accounts in the files group, on every node; the Users page still manages administrators only. Every signed-in end user sees every synced folder.

## 1

First release of filedir, a file distribution system, forked from logdir 32 (framework) and built around a new sync engine.

- **Sync engine** (`source/filesync`, standard library only). Version vectors, per-peer cursors, a journaled index, block hashes.
  Concurrent edits resolve deterministically (a change beats a delete, then the later mtime, then the higher node id) and the loser
  is kept as a `sync-conflict` copy. A 5-node random soak test converges (200 seeds run).
- **Swarm fetch.** A node pulls the blocks of a file in parallel (up to 8) from every node that holds that version, verifies each
  block's hash, and falls back to another peer on failure. Not yet: serving partial files, rarest-first.
- **Safety.** `.filedir` marker, mass-delete guard, name validation, symlink refusal, `O_NOFOLLOW` reads, trash (30 days) and
  tombstones (180 days). A file changed locally during a pull is never overwritten.
- **Cluster.** Transfers between nodes are TLS with pinned certificates and signed requests, on a port of their own (4452). GUI 4450,
  cluster 4451, gateway ids start at 137.
- **Web file manager** (Files page): browse, upload and drag-drop, download, edit text, rename, make folders, delete; right-click
  menus on every file and folder. Configure ▸ Folders and Monitor ▸ Files.
- **MCP** (read-only): `files_status`, `files_list`, `list_nodes`, `get_topology`.
- **Removed from logdir:** the syslog collector, senders, the Logs and Senders pages, `logs_*` tools and settings
  (`settings.logs`, `log_listen`, `log_dir`, `topology_squares_above`). Daemon log forwarding to a syslog server stays.
- **Not verified here:** real PAM sign-in (the sandbox refused creating a test user), IPv6 binds. Notes in `docs/DESIGN.md`.
