# Quick start

From nothing to a folder shared between two nodes.

## 1. Install

```
tar xzf filedir_v1.tgz && cd filedir
sudo ./install.sh --add-user "$USER"
```

## 2. Create the gateway

In the web GUI (`https://NODE:4450`, signed in as the user you added): Topology ▸ **＋ New gateway**. The gateway is one shared address (its VIP) that the assistant uses. File transfer between nodes doesn't need it.

Or by hand:

```
sudo cp /usr/local/share/filedir/filedir.conf.example /var/lib/filedir/filedir.conf
sudo chmod 600 /var/lib/filedir/filedir.conf
sudo editor /var/lib/filedir/filedir.conf
```

Set:

- `groups[0].interface` and `vip4`: an unused address on that interface's subnet
- a long random `key`
- `settings.mcp_acl` to the address of the machine the assistant runs on

The daemon picks the file up by itself. Check:

```
filedir --show-gateways
filedir --canvas
```

## 3. Add a folder

Configure ▸ **Folders** ▸ right-click ▸ Add row: an id (`docs`, say) and a path on this node (`/srv/docs`). The directory is created with its `.filedir` marker. Files you put there are picked up within a second or two, and the **Files** page (the first entry of the menu) shows them. There are no buttons: right-click anywhere in it to upload, rename, edit, delete or download as a zip, select several with Ctrl+click or Shift+click, and drop files or folders from your computer onto it.

To give other people the Files page alone (no cluster settings), put their accounts in a group on every node (or in your directory), put that group's name in **Files-only groups** under Configure ▸ Web GUI, and open `https://<gateway address>/`. The Full-access and Read-only groups may use that page too.

## 4. Connect an assistant

Install a certificate the client trusts (`filedir --tls-install --cert-file F --key-file F`), then, for Claude Code:

```
claude mcp add --transport http filedir https://VIP/mcp
```

Ask it to `files_status` to see whether every node holds every folder, or to `files_list` a folder, `files_read` a file, or (with a read-write key) `files_write` one. For a big file ask for `files_download_url` or `files_upload_url` and use the link with `curl`.

## 5. More nodes

On a second node, install it the same way. Then run `filedir --cluster-token` on the first node and `filedir --cluster-join CODE` on the second (cluster port 4451). The gateway, the folder list and the access lists come across. Add the same folder id on the second node (its path may differ) and the files appear there. File transfer between nodes uses port 4452, so open it between the nodes.

Every node holds a full copy and fetches blocks from every node that has them. On links with high latency between nodes, raise the gateway timers (`hello_ms` 500, `hold_ms` 3000) so a slow link does not flip the gateway.

**WebDAV.** Map `https://<files address>/dav/` as a network drive (Windows: Add a network location; Mac: Finder ▸ Go ▸ Connect to Server) and sign in with the same name and password as the files site.
