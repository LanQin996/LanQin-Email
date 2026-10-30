package online

import (
	"context"
	"errors"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"
	"unicode"
)

var ErrMaildirAmbiguous = errors.New("ambiguous Maildir file identity")

type MaildirLimits struct {
	MaxEntries   int // All enumerated entries, including ignored metadata.
	MaxFiles     int
	MaxPathBytes int
}

func DefaultMaildirLimits() MaildirLimits {
	return MaildirLimits{MaxEntries: 1000000, MaxFiles: 100000, MaxPathBytes: 4096}
}

// MaildirEntry identifies content in a single mailbox's namespace. Identity is
// only the immutable filename portion, never the MIME Message-ID. Catch-all mail
// has its own domain/__unregistered__/Maildir namespace, not another user's.
type MaildirEntry struct {
	Path     string
	Mailbox  string
	Identity string
	Bytes    int64
	ModTime  time.Time   `json:"-"`
	Info     os.FileInfo `json:"-"`
}

// ScanMaildir inventories only domain/local/Maildir/{new,cur} and one-level
// Maildir++ .folder/{new,cur}. It does not read MIME content, follow links, scan
// tmp, or copy Dovecot indexes. The caller must copy pinned files and reconcile
// unindexed deliveries separately; an inventory is not a point-in-time snapshot.
func ScanMaildir(ctx context.Context, root *os.Root, limits MaildirLimits) ([]MaildirEntry, error) {
	if root == nil || limits.MaxEntries < 1 || limits.MaxEntries > 10000000 || limits.MaxFiles < 1 || limits.MaxFiles > 1000000 || limits.MaxFiles > limits.MaxEntries || limits.MaxPathBytes < 1 || limits.MaxPathBytes > 65536 {
		return nil, ErrFileLimit
	}
	count := 0
	entries := []MaildirEntry{}
	var scan func(*os.Root, string, int) error
	scan = func(parent *os.Root, prefix string, level int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		dir, err := parent.Open(".")
		if err != nil {
			return ErrSourceChanged
		}
		defer dir.Close()
		for {
			children, readErr := dir.ReadDir(64)
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return ErrSourceChanged
			}
			for _, child := range children {
				if err := ctx.Err(); err != nil {
					return err
				}
				count++
				if count > limits.MaxEntries {
					return ErrFileLimit
				}
				name := child.Name()
				if !maildirComponent(name) {
					return ErrUnsafePath
				}
				rel := name
				if prefix != "" {
					rel = prefix + "/" + name
				}
				if len(rel) > limits.MaxPathBytes {
					return ErrFileLimit
				}
				next, relevant := maildirTraversal(level, name)
				if !relevant {
					continue
				}
				info, err := parent.Lstat(name)
				if err != nil {
					return ErrSourceChanged
				}
				if info.Mode()&os.ModeSymlink != 0 {
					return ErrUnsafePath
				}
				if level == 5 {
					if info.IsDir() {
						continue // Never descend into a directory inside new/cur.
					}
					if !info.Mode().IsRegular() {
						return ErrUnsafePath
					}
					// File.Stat captures identity through the open handle. On
					// Windows, Lstat may defer loading the file ID until SameFile,
					// which would otherwise observe a replacement at a later time.
					file, err := parent.Open(name)
					if err != nil {
						return ErrSourceChanged
					}
					opened, statErr := file.Stat()
					current, currentErr := parent.Lstat(name)
					closeErr := file.Close()
					if statErr != nil || currentErr != nil || closeErr != nil || !sameStableFile(info, opened) || !sameStableFile(opened, current) {
						return ErrSourceChanged
					}
					entry, err := parseMaildirPath(rel)
					if err != nil {
						return err
					}
					if len(entries) >= limits.MaxFiles || info.Size() < 0 {
						return ErrFileLimit
					}
					entry.Bytes = opened.Size()
					entry.ModTime = opened.ModTime()
					entry.Info = opened
					entries = append(entries, entry)
					continue
				}
				if !info.IsDir() {
					continue
				}
				// Opening relative to a pinned parent root avoids traversing an
				// ancestor swapped to a link during the inventory operation.
				sub, err := parent.OpenRoot(name)
				if err != nil {
					return ErrSourceChanged
				}
				opened, statErr := sub.Stat(".")
				if statErr != nil || !opened.IsDir() || !os.SameFile(info, opened) {
					sub.Close()
					return ErrSourceChanged
				}
				scanErr := scan(sub, rel, next)
				current, currentErr := parent.Lstat(name)
				closeErr := sub.Close()
				if scanErr != nil {
					return scanErr
				}
				if currentErr != nil || closeErr != nil || !current.IsDir() || !os.SameFile(info, current) {
					return ErrSourceChanged
				}
			}
			if errors.Is(readErr, io.EOF) {
				return nil
			}
		}
	}
	if err := scan(root, "", 0); err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

// Levels: root/domain/local/Maildir/.folder/new-or-cur. Unregistered catch-all
// directories use exactly the same physical layout and need no special fallback.
func maildirTraversal(level int, name string) (int, bool) {
	switch level {
	case 0, 1:
		return level + 1, !strings.HasPrefix(name, ".")
	case 2:
		return 3, name == "Maildir"
	case 3:
		if name == "new" || name == "cur" {
			return 5, true
		}
		return 4, strings.HasPrefix(name, ".") && name != "." && name != ".."
	case 4:
		return 5, name == "new" || name == "cur"
	case 5:
		return 0, !strings.HasPrefix(name, ".")
	}
	return 0, false
}

func maildirComponent(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\\x00") && strings.IndexFunc(name, unicode.IsControl) < 0
}

func parseMaildirPath(relative string) (MaildirEntry, error) {
	var empty MaildirEntry
	if relative == "" || len(relative) > 65536 || path.Clean(relative) != relative || strings.HasPrefix(relative, "/") || strings.Contains(relative, "\\") {
		return empty, ErrUnsafePath
	}
	parts := strings.Split(relative, "/")
	if len(parts) != 5 && len(parts) != 6 {
		return empty, ErrUnsafePath
	}
	for _, part := range parts {
		if !maildirComponent(part) {
			return empty, ErrUnsafePath
		}
	}
	if strings.HasPrefix(parts[0], ".") || strings.HasPrefix(parts[1], ".") || strings.ContainsAny(parts[0]+parts[1], ":") || parts[2] != "Maildir" {
		return empty, ErrUnsafePath
	}
	sub := 3
	if len(parts) == 6 {
		if !strings.HasPrefix(parts[3], ".") || strings.HasPrefix(parts[3], "..") || strings.Contains(parts[3], ":") {
			return empty, ErrUnsafePath
		}
		sub++
	}
	if parts[sub] != "new" && parts[sub] != "cur" {
		return empty, ErrUnsafePath
	}
	filename := parts[sub+1]
	if strings.HasPrefix(filename, ".") {
		return empty, ErrUnsafePath
	}
	identity := filename
	sep := -1
	for _, marker := range []string{":2,", "!2,"} {
		if i := strings.LastIndex(filename, marker); i > sep {
			sep = i
		}
	}
	if sep >= 0 {
		for _, flag := range filename[sep+3:] {
			if (flag < 'A' || flag > 'Z') && (flag < 'a' || flag > 'z') {
				return empty, ErrUnsafePath
			}
		}
		identity = filename[:sep]
	}
	if identity == "" || strings.ContainsAny(identity, ":") {
		return empty, ErrUnsafePath
	}
	return MaildirEntry{Path: relative, Mailbox: strings.Join(parts[:3], "/"), Identity: identity}, nil
}

type MaildirIndex struct {
	byIdentity map[string][]MaildirEntry
}

// NewMaildirIndex validates the supplied inventory once for efficient batch
// resolution. The index is a private copy, unaffected by caller slice mutation.
func NewMaildirIndex(inventory []MaildirEntry) (*MaildirIndex, error) {
	if len(inventory) > 1000000 {
		return nil, ErrFileLimit
	}
	index := &MaildirIndex{byIdentity: map[string][]MaildirEntry{}}
	for _, supplied := range inventory {
		entry, err := parseMaildirPath(supplied.Path)
		if err != nil || supplied.Mailbox != entry.Mailbox || supplied.Identity != entry.Identity || supplied.Bytes < 0 {
			return nil, ErrUnsafePath
		}
		if supplied.Info != nil && (!supplied.Info.Mode().IsRegular() || supplied.Info.Size() != supplied.Bytes || !supplied.Info.ModTime().Equal(supplied.ModTime)) {
			return nil, ErrUnsafePath
		}
		entry.Bytes = supplied.Bytes
		entry.ModTime = supplied.ModTime
		entry.Info = supplied.Info
		key := entry.Mailbox + "\x00" + entry.Identity
		index.byIdentity[key] = append(index.byIdentity[key], entry)
	}
	return index, nil
}

// ResolveReference allows flag and folder renames only within the exact original
// mailbox namespace. Multiple matching identities fail even when one is an exact
// path match: guessing risks selecting a different MIME version. Absence is
// retryable, never a reason to silently drop the database's referenced message.
func (index *MaildirIndex) ResolveReference(relative string) (MaildirEntry, error) {
	reference, err := parseMaildirPath(relative)
	if err != nil {
		return MaildirEntry{}, err
	}
	if index == nil {
		return MaildirEntry{}, ErrSourceMissing
	}
	candidates := index.byIdentity[reference.Mailbox+"\x00"+reference.Identity]
	if len(candidates) == 0 {
		return MaildirEntry{}, ErrSourceMissing
	}
	if len(candidates) != 1 {
		return MaildirEntry{}, ErrMaildirAmbiguous
	}
	return candidates[0], nil
}

// ResolveMaildirReference is a convenience wrapper. Batch callers should build
// one NewMaildirIndex and use its O(1) ResolveReference method instead.
func ResolveMaildirReference(relative string, inventory []MaildirEntry) (MaildirEntry, error) {
	index, err := NewMaildirIndex(inventory)
	if err != nil {
		return MaildirEntry{}, err
	}
	return index.ResolveReference(relative)
}
