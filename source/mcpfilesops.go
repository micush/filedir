package main

import (
	"errors"
	"path"
	"strings"

	"filedir/filesync"
)

// The MCP tools that change the tree of a synced folder, besides files_write and files_upload_url: files_mkdir, files_delete, files_move
// and files_copy.  They are the Files page's own operations (fd_files_api.go) for a read-write caller.  Nothing is removed for good: a
// delete, and a file that a move or a copy replaces, goes to the folder's trash on the node that took the call (30 days by default).
// Settings, folders and users are still changed in the GUI only.

// mcpFolderOf is the synced folder a tool names, or an error that says what is wrong.
func mcpFolderOf(c *mcpCtx, key string) (*fdFolder, error) {
	if fdSvc == nil {
		return nil, errors.New("file sync is not running")
	}
	f := fdSvc.get(c.str(key))
	if f == nil || f.f == nil {
		return nil, errors.New("no such folder on this node")
	}
	return f, nil
}

func mcpFilesMkdir(c *mcpCtx) (any, error) {
	f, err := mcpFolderOf(c, "folder")
	if err != nil {
		return nil, err
	}
	p := c.str("path")
	if p == "" {
		return nil, errors.New("path is required")
	}
	if err := f.f.Mkdir(p); err != nil {
		return nil, errors.New(fdErrText(err))
	}
	f.rescan()
	fdAudit(c.s, "files: %s: made folder %s over MCP", f.cfg.ID, p)
	return map[string]any{"folder": c.str("folder"), "path": p, "made": true}, nil
}

func mcpFilesDelete(c *mcpCtx) (any, error) {
	f, err := mcpFolderOf(c, "folder")
	if err != nil {
		return nil, err
	}
	p := strings.Trim(path.Clean("/"+c.str("path")), "/")
	if p == "" {
		return nil, errors.New("path is required (the folder itself cannot be deleted)")
	}
	if err := f.f.Delete(p); err != nil {
		return nil, errors.New(fdErrText(err))
	}
	f.rescan()
	fdAudit(c.s, "files: %s: deleted %s over MCP (kept in the trash)", f.cfg.ID, p)
	return map[string]any{"folder": c.str("folder"), "path": p, "deleted": true, "note": "kept in the folder's trash on the node that took the call"}, nil
}

// mcpFilesCopyMove copies or moves a file or a folder tree, inside a folder or to another one, as the Files page does without its
// background mode.  to_folder defaults to the same folder; the parent of "to" must exist (files_mkdir makes it); a name that is taken is
// refused, unless "unique" asks for a free one ("name copy.ext").
func mcpFilesCopyMove(c *mcpCtx, move bool) (any, error) {
	src, err := mcpFolderOf(c, "folder")
	if err != nil {
		return nil, err
	}
	dst := src
	if tf := c.str("to_folder"); tf != "" && tf != c.str("folder") {
		if dst, err = mcpFolderOf(c, "to_folder"); err != nil {
			return nil, err
		}
	}
	unique, _ := c.flag("unique")
	from, to := strings.Trim(path.Clean("/"+c.str("path")), "/"), strings.Trim(path.Clean("/"+c.str("to")), "/")
	if from == "" || to == "" {
		return nil, errors.New("path and to are required, and neither can be the folder itself")
	}
	if src == dst && (strings.HasPrefix(to, from+"/") || (to == from && (move || !unique))) {
		return nil, errors.New("that cannot be put into itself")
	}
	if _, err := src.f.Stat(from); err != nil {
		return nil, errors.New(fdErrText(err))
	}
	if p := path.Dir(to); p != "." {
		if st, err := dst.f.Stat(p); err != nil || !st.IsDir() {
			return nil, errors.New("the folder it goes into is not there (make it with files_mkdir)")
		}
	}
	if _, err := dst.f.Stat(to); err == nil {
		if !unique {
			return nil, errors.New("that name is already there (unique:true picks a free name)")
		}
		if to, err = fdFreeName(dst, to); err != nil {
			return nil, errors.New(fdErrText(err))
		}
	}
	if move && src == dst {
		err = src.f.Rename(from, to)
	} else {
		a, b := davTarget{f: src, id: src.cfg.ID, rel: from}, davTarget{f: dst, id: dst.cfg.ID, rel: to}
		if err = davCopyTree(a, b, false); err == nil && move {
			err = src.f.Delete(from)
		}
	}
	if err != nil {
		return nil, errors.New(fdErrText(err))
	}
	src.rescan()
	dst.rescan()
	what := map[bool]string{true: "moved", false: "copied"}[move]
	fdAudit(c.s, "files: %s: %s %s/%s to %s/%s over MCP", dst.cfg.ID, what, src.cfg.ID, from, dst.cfg.ID, to)
	return map[string]any{"folder": src.cfg.ID, "path": from, "to_folder": dst.cfg.ID, "to": to, what: true}, nil
}

// The trash of a folder is kept by each node on its own (a delete goes to the trash of the node that took it), so these tools work on the
// trash of the node that answers the call.

func mcpFilesTrash(c *mcpCtx) (any, error) {
	f, err := mcpFolderOf(c, "folder")
	if err != nil {
		return nil, err
	}
	b, n := f.f.TrashSize()
	items, total, err := f.f.TrashEntries(c.str("when"), c.str("path"), 1000)
	if err != nil {
		return nil, errors.New(fdErrText(err))
	}
	if items == nil {
		items = []filesync.TrashEntry{}
	}
	return map[string]any{"folder": c.str("folder"), "bytes": b, "files": n, "entries": items, "total": total, "truncated": total > len(items),
		"note": "this is the trash of the node that answered; each node keeps its own"}, nil
}

func mcpFilesTrashEmpty(c *mcpCtx) (any, error) {
	f, err := mcpFolderOf(c, "folder")
	if err != nil {
		return nil, err
	}
	if ok, _ := c.flag("confirm"); !ok {
		return nil, errors.New("this removes what is in the trash for good, on the node that answers; ask again with confirm: true to do it")
	}
	b, n, err := f.f.EmptyTrash()
	if err != nil {
		return nil, errors.New(fdErrText(err))
	}
	fdAudit(c.s, "files: %s: emptied the trash over MCP (%d files, %d bytes)", f.cfg.ID, n, b)
	return map[string]any{"folder": c.str("folder"), "bytes": b, "files": n, "emptied": true}, nil
}

func mcpFilesTrashRestore(c *mcpCtx) (any, error) {
	f, err := mcpFolderOf(c, "folder")
	if err != nil {
		return nil, err
	}
	when, name := c.str("when"), c.str("name")
	if when == "" || name == "" {
		return nil, errors.New("when and name are required (as files_trash lists them)")
	}
	n, err := f.f.RestoreTrash(when, name)
	if err != nil {
		return nil, errors.New(fdErrText(err))
	}
	f.rescan()
	fdAudit(c.s, "files: %s: restored %s from the trash over MCP (%d files)", f.cfg.ID, name, n)
	return map[string]any{"folder": c.str("folder"), "name": name, "files": n, "restored": true}, nil
}
