// Package fidelity is the metadata fixture and its comparison: every metadata class a Linux file backup
// must keep, and a check of all 13 properties after a restore. The e2e suite uses it, and so does lab/fidelity,
// which runs it on the lab's Ubuntu, AlmaLinux and Oracle Linux VMs. It needs root.
package fidelity

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// POSIX ACL xattr encoding (linux/posix_acl_xattr.h), written without setfacl so the fixture works on
// minimal images.
const (
	aclUserObj  = 0x01
	aclUser     = 0x02
	aclGroupObj = 0x04
	aclGroup    = 0x08
	aclMask     = 0x10
	aclOther    = 0x20
	aclUndefID  = 0xffffffff
)

type aclEntry struct {
	tag  uint16
	perm uint16
	id   uint32
}

func encodeACL(entries ...aclEntry) []byte {
	b := make([]byte, 4, 4+8*len(entries))
	binary.LittleEndian.PutUint32(b, 2)
	for _, e := range entries {
		var x [8]byte
		binary.LittleEndian.PutUint16(x[0:], e.tag)
		binary.LittleEndian.PutUint16(x[2:], e.perm)
		binary.LittleEndian.PutUint32(x[4:], e.id)
		b = append(b, x[:]...)
	}
	return b
}

var fixtureMtime = time.Date(2001, 2, 3, 4, 5, 6, 123456789, time.UTC)

// Build creates, under root, a tree with every metadata class a Linux file backup must keep.
func Build(root string) error {
	var errs []error
	try := func(err error) { errs = append(errs, err) }
	w := func(name, content string, mode os.FileMode) string {
		p := filepath.Join(root, name)
		try(os.WriteFile(p, []byte(content), mode))
		try(os.Chmod(p, mode))
		return p
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	w("plain.txt", "plain\n", 0o644)
	p := w("owned.txt", "owned by 1234:5678\n", 0o640)
	try(os.Chown(p, 1234, 5678))
	p = w("setuid-bin", "#!/bin/sh\n", 0o755)
	try(os.Chmod(p, 0o755|os.ModeSetuid))
	p = w("setgid-bin", "#!/bin/sh\n", 0o755)
	try(os.Chmod(p, 0o755|os.ModeSetgid))
	sd := filepath.Join(root, "sticky-dir")
	try(os.Mkdir(sd, 0o777))
	try(os.Chmod(sd, 0o777|os.ModeSticky))

	p = w("acl-file.txt", "acl\n", 0o640)
	try(syscall.Setxattr(p, "system.posix_acl_access", encodeACL(
		aclEntry{aclUserObj, 6, aclUndefID}, aclEntry{aclUser, 4, 1234}, aclEntry{aclGroupObj, 4, aclUndefID},
		aclEntry{aclGroup, 6, 5678}, aclEntry{aclMask, 6, aclUndefID}, aclEntry{aclOther, 0, aclUndefID}), 0))
	ad := filepath.Join(root, "acl-dir")
	try(os.Mkdir(ad, 0o750))
	try(syscall.Setxattr(ad, "system.posix_acl_default", encodeACL(
		aclEntry{aclUserObj, 7, aclUndefID}, aclEntry{aclUser, 5, 1234}, aclEntry{aclGroupObj, 5, aclUndefID},
		aclEntry{aclMask, 5, aclUndefID}, aclEntry{aclOther, 0, aclUndefID}), 0))
	try(os.WriteFile(filepath.Join(ad, "inherits.txt"), []byte("inherits default acl\n"), 0o640))

	p = w("xattr-file.txt", "xattrs\n", 0o644)
	try(syscall.Setxattr(p, "user.comment", []byte("hello holdfast"), 0))
	try(syscall.Setxattr(p, "user.binary", []byte{0, 1, 2, 255}, 0))
	try(syscall.Setxattr(p, "trusted.app", []byte("root-only namespace"), 0))
	p = w("selinux-file.txt", "labelled\n", 0o644)
	try(syscall.Setxattr(p, "security.selinux", []byte("system_u:object_r:httpd_sys_content_t:s0\x00"), 0))

	a := w("hardlink-a", "same inode\n", 0o644)
	try(os.Link(a, filepath.Join(root, "hardlink-b")))
	try(os.Symlink("plain.txt", filepath.Join(root, "symlink")))
	try(os.Lchown(filepath.Join(root, "symlink"), 1234, 5678))
	try(os.Symlink("/nonexistent/target", filepath.Join(root, "dangling-symlink")))

	if f, err := os.Create(filepath.Join(root, "sparse.img")); err != nil {
		try(err)
	} else {
		try(f.Truncate(64 << 20))
		_, err = f.WriteAt([]byte(strings.Repeat("D", 4096)), 32<<20)
		try(err)
		try(f.Close())
	}

	w("name with spaces & ünïcødé.txt", "unicode\n", 0o644)
	w("newline\nin-name.txt", "newline\n", 0o644)
	try(os.Mkdir(filepath.Join(root, "empty-dir"), 0o700))
	try(syscall.Mkfifo(filepath.Join(root, "fifo"), 0o600))
	p = w("old-mtime.txt", "old\n", 0o644)
	try(os.Chtimes(p, fixtureMtime, fixtureMtime))
	// Directory mtimes last, after their children exist.
	try(os.Chtimes(ad, fixtureMtime, fixtureMtime))
	try(os.Chtimes(filepath.Join(root, "empty-dir"), fixtureMtime, fixtureMtime))
	return errors.Join(errs...)
}

// Entry is what a scan records of one path.
type Entry struct {
	kind   string
	mode   uint32
	uid    uint32
	gid    uint32
	size   int64
	blocks int64
	mtime  int64
	link   string
	xattrs map[string]string
	ino    uint64
	sum    string
}

func kindOf(m uint32) string {
	switch m & syscall.S_IFMT {
	case syscall.S_IFREG:
		return "file"
	case syscall.S_IFDIR:
		return "dir"
	case syscall.S_IFLNK:
		return "symlink"
	case syscall.S_IFIFO:
		return "fifo"
	}
	return "other"
}

func xattrs(path string) map[string]string {
	out := map[string]string{}
	sz, err := syscall.Listxattr(path, nil)
	if err != nil || sz == 0 {
		return out
	}
	buf := make([]byte, sz)
	sz, err = syscall.Listxattr(path, buf)
	if err != nil {
		return out
	}
	for _, name := range strings.Split(strings.TrimRight(string(buf[:sz]), "\x00"), "\x00") {
		if name == "" {
			continue
		}
		vs, err := syscall.Getxattr(path, name, nil)
		if err != nil {
			continue
		}
		v := make([]byte, vs)
		vs, _ = syscall.Getxattr(path, name, v)
		out[name] = hex.EncodeToString(v[:vs])
	}
	return out
}

// Scan records every path under root.
func Scan(root string) (map[string]Entry, error) {
	out := map[string]Entry{}
	err := filepath.Walk(root, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		var st syscall.Stat_t
		if err := syscall.Lstat(p, &st); err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		e := Entry{kind: kindOf(st.Mode), mode: st.Mode & 0o7777, uid: st.Uid, gid: st.Gid, size: st.Size, blocks: st.Blocks, mtime: st.Mtim.Nano(), ino: st.Ino}
		switch e.kind {
		case "symlink":
			e.link, _ = os.Readlink(p)
		case "file":
			if f, err := os.Open(p); err == nil {
				h := sha256.New()
				_, _ = io.Copy(h, f)
				f.Close()
				e.sum = hex.EncodeToString(h.Sum(nil))
			}
			e.xattrs = xattrs(p)
		default:
			e.xattrs = xattrs(p)
		}
		out[rel] = e
		return nil
	})
	return out, err
}

// Result is one row of the fidelity report.
type Result struct {
	Property string `json:"property"`
	Pass     bool   `json:"pass"`
	Detail   string `json:"detail"`
}

// Compare checks the 13 properties of a restored tree against its source.
func Compare(src, dst map[string]Entry) []Result {
	type check struct{ fail []string }
	order := []string{"file set and names (unicode, spaces, newline)", "file content (sha256)", "file types incl. FIFO",
		"owner uid/gid incl. symlink owner", "permission bits incl. setuid, setgid, sticky",
		"mtime (nanoseconds) incl. directories", "symlink targets incl. dangling",
		"user.* and trusted.* xattrs", "POSIX access ACLs", "POSIX default ACLs on directories",
		"security.selinux label", "hard links (shared inode)", "sparse file holes"}
	checks := map[string]*check{}
	for _, n := range order {
		checks[n] = &check{}
	}
	add := func(n, msg string) { checks[n].fail = append(checks[n].fail, msg) }
	for rel, s := range src {
		d, ok := dst[rel]
		if !ok {
			add(order[0], "missing: "+rel)
			continue
		}
		if s.kind != d.kind {
			add(order[2], fmt.Sprintf("%s: %s -> %s", rel, s.kind, d.kind))
			continue
		}
		if s.sum != d.sum {
			add(order[1], rel)
		}
		if s.uid != d.uid || s.gid != d.gid {
			add(order[3], fmt.Sprintf("%s: %d:%d -> %d:%d", rel, s.uid, s.gid, d.uid, d.gid))
		}
		if s.kind != "symlink" && s.mode != d.mode {
			add(order[4], fmt.Sprintf("%s: %04o -> %04o", rel, s.mode, d.mode))
		}
		if s.kind != "symlink" && rel != "." && s.mtime != d.mtime {
			add(order[5], fmt.Sprintf("%s: %s -> %s", rel, time.Unix(0, s.mtime).UTC().Format(time.RFC3339Nano), time.Unix(0, d.mtime).UTC().Format(time.RFC3339Nano)))
		}
		if s.link != d.link {
			add(order[6], fmt.Sprintf("%s: %q -> %q", rel, s.link, d.link))
		}
		for k, v := range s.xattrs {
			target := order[7]
			switch k {
			case "system.posix_acl_access":
				target = order[8]
			case "system.posix_acl_default":
				target = order[9]
			case "security.selinux":
				target = order[10]
			}
			if d.xattrs[k] != v {
				add(target, fmt.Sprintf("%s: %s differs", rel, k))
			}
		}
		// restic restores holes at chunk granularity, so the criterion is "holes kept": allocation under
		// 25% of the apparent size.
		if rel == "sparse.img" && d.blocks*512 > d.size/4 {
			add(order[12], fmt.Sprintf("holes lost: %d KiB allocated for %d MiB file", d.blocks/2, d.size>>20))
		}
	}
	for rel := range dst {
		if _, ok := src[rel]; !ok {
			add(order[0], "unexpected: "+rel)
		}
	}
	byIno := map[uint64][]string{}
	for rel, s := range src {
		if s.kind == "file" {
			byIno[s.ino] = append(byIno[s.ino], rel)
		}
	}
	for _, group := range byIno {
		if len(group) < 2 {
			continue
		}
		sort.Strings(group)
		first := dst[group[0]].ino
		for _, rel := range group[1:] {
			if dst[rel].ino != first {
				add(order[11], fmt.Sprintf("%v no longer share an inode", group))
			}
		}
	}
	var out []Result
	for _, n := range order {
		c := checks[n]
		r := Result{Property: n, Pass: len(c.fail) == 0, Detail: "identical"}
		if !r.Pass {
			sort.Strings(c.fail)
			if len(c.fail) > 4 {
				c.fail = append(c.fail[:4], fmt.Sprintf("... %d more", len(c.fail)-4))
			}
			r.Detail = strings.Join(c.fail, "; ")
		}
		out = append(out, r)
	}
	return out
}
