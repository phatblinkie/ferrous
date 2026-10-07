package docker

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// Phase 5: scoped file access to a container's data directory — the panel's
// file browser/editor (server.cfg, oxide plugins). All paths are interpreted
// relative to the resolved root; absolute inputs, ".." escapes and symlinks
// leaving the root are rejected. The check is TOCTOU-racy by design: the
// agent token is already root-equivalent (docker socket), so an admin racing
// their own symlink gains nothing.

var (
	// ErrNotFound: path (or the data root) does not exist.
	ErrNotFound = errors.New("not found")
	// ErrEscape: path would leave the data directory.
	ErrEscape = errors.New("path escapes the data directory")
	// ErrTooLarge: file exceeds MaxFileBytes.
	ErrTooLarge = errors.New("file too large (8 MB limit)")
	// ErrBadPath: client path is malformed (absolute, or contains NUL).
	ErrBadPath = errors.New("path must be relative to the data directory")
	// ErrIsDir / ErrNotDir: wrong path kind for the operation.
	ErrIsDir  = errors.New("is a directory")
	ErrNotDir = errors.New("not a directory")
)

// MaxFileBytes caps reads and writes (configs are tiny; oxide plugins are
// a few MB — 8 MB covers both without letting the panel become a blob store).
const MaxFileBytes = 8 << 20

// DataRoot resolves the host directory that backs a container's data:
//  1. the `ferrous.datadir` label (absolute host path — set by deploy),
//  2. else the container's single writable mount (common single-bind case),
//  3. else an error telling the operator to set the label (ambiguous).
func DataRoot(d *ContainerDetail) (string, error) {
	if v := strings.TrimSpace(d.Config.Labels["ferrous.datadir"]); v != "" {
		if !filepath.IsAbs(v) {
			return "", fmt.Errorf("ferrous.datadir label must be absolute: %q", v)
		}
		v = filepath.Clean(v)
		if v == string(filepath.Separator) {
			return "", errors.New("ferrous.datadir refuses / as a data directory")
		}
		return v, nil
	}
	var roots []string
	for _, m := range d.Mounts {
		if !m.RW || m.Source == "" {
			continue
		}
		roots = append(roots, m.Source)
	}
	switch len(roots) {
	case 0:
		return "", errors.New("container has no writable mount — set the ferrous.datadir label")
	case 1:
		return filepath.Clean(roots[0]), nil
	default:
		return "", fmt.Errorf("container has %d writable mounts — set the ferrous.datadir label to pick one", len(roots))
	}
}

// FileEntry is one directory row.
type FileEntry struct {
	Name  string `json:"name"`
	Dir   bool   `json:"dir"`
	Size  int64  `json:"size"`
	MTime string `json:"mtime"` // RFC3339
}

// FileInfo is the files-API response: a directory (entries) or a file
// (content, utf8 or base64).
type FileInfo struct {
	Path     string      `json:"path"` // cleaned, "" = data root
	Type     string      `json:"type"` // "dir" | "file"
	Size     int64       `json:"size,omitempty"`
	MTime    string      `json:"mtime,omitempty"`
	Encoding string      `json:"encoding,omitempty"` // "utf8" | "base64"
	Content  string      `json:"content,omitempty"`
	Entries  []FileEntry `json:"entries,omitempty"`
}

// FileService performs path-scoped reads/writes under an already-resolved
// absolute root (symlinks in the root path itself are folded in at creation).
type FileService struct {
	root string
}

// NewFileService binds to root; the root must exist and be a directory.
// "/" is rejected: it would make the files API the whole host filesystem.
func NewFileService(root string) (*FileService, error) {
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: data directory %s", ErrNotFound, root)
		}
		return nil, err
	}
	if real == string(filepath.Separator) {
		return nil, fmt.Errorf("%w: refusing / as a data directory", ErrNotDir)
	}
	st, err := os.Stat(real)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%w: data directory %s is not a directory", ErrNotDir, root)
	}
	return &FileService{root: real}, nil
}

// Root returns the resolved root (for logs/tests).
func (fs *FileService) Root() string { return fs.root }

// resolve maps a client path onto an absolute path under root.
// missingOK: the deepest existing ancestor is verified instead of the full
// path (for writes to files/dirs that don't exist yet).
func (fs *FileService) resolve(rel string, missingOK bool) (string, error) {
	if strings.ContainsRune(rel, 0) {
		return "", fmt.Errorf("%w (invalid path)", ErrBadPath)
	}
	if rel == "" || rel == "/" {
		return fs.root, nil
	}
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, `\`) {
		return "", ErrBadPath
	}
	for _, e := range strings.Split(rel, string(filepath.Separator)) {
		if e == ".." {
			return "", ErrEscape
		}
	}
	// Clean("/"+rel) folds "./" and duplicates without ever escaping.
	cleaned := strings.TrimPrefix(filepath.Clean("/"+rel), "/")
	if cleaned == "." || cleaned == "" {
		return fs.root, nil
	}
	full := filepath.Join(fs.root, cleaned)

	if missingOK {
		// verify the deepest existing ancestor (the file itself may be new);
		// walking up stops at root, which NewFileService already validated
		p := full
		for {
			real, err := filepath.EvalSymlinks(p)
			if err == nil {
				if !under(real, fs.root) {
					return "", ErrEscape
				}
				break
			}
			if !os.IsNotExist(err) {
				return "", mapPathErr(err)
			}
			if p == fs.root || p == filepath.Dir(p) {
				break // reached an existing point (or the root vanished — write fails below)
			}
			p = filepath.Dir(p)
		}
		return full, nil
	}

	real, err := filepath.EvalSymlinks(full)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%w: %s", ErrNotFound, cleaned)
		}
		return "", mapPathErr(err)
	}
	if !under(real, fs.root) {
		return "", ErrEscape
	}
	return full, nil
}

// mapPathErr turns the engine of path errors into our sentinels: a non-dir
// in the chain ("afile/child") is a client mistake (400), not a 500.
func mapPathErr(err error) error {
	if errors.Is(err, syscall.ENOTDIR) {
		return ErrNotDir
	}
	return err
}

func under(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

// Get returns a directory listing or file content for rel.
func (fs *FileService) Get(rel string) (*FileInfo, error) {
	abs, err := fs.resolve(rel, false)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, rel)
		}
		return nil, err
	}
	if st.IsDir() {
		return fs.list(rel, abs, st)
	}
	if st.Size() > MaxFileBytes {
		return nil, ErrTooLarge
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, rel)
		}
		return nil, err
	}
	enc := encodingOf(b)
	return &FileInfo{
		Path: cleanRel(rel), Type: "file",
		Size: st.Size(), MTime: st.ModTime().UTC().Format(time.RFC3339),
		Encoding: enc, Content: contentOf(b, enc),
	}, nil
}

func (fs *FileService) list(rel, abs string, st os.FileInfo) (*FileInfo, error) {
	des, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}
	out := &FileInfo{Path: cleanRel(rel), Type: "dir", MTime: st.ModTime().UTC().Format(time.RFC3339)}
	for _, de := range des {
		e := FileEntry{Name: de.Name(), Dir: de.IsDir()}
		if info, err := de.Info(); err == nil {
			if !de.IsDir() {
				e.Size = info.Size()
			}
			e.MTime = info.ModTime().UTC().Format(time.RFC3339)
		}
		out.Entries = append(out.Entries, e)
	}
	sort.Slice(out.Entries, func(i, j int) bool {
		if a, b := out.Entries[i].Dir, out.Entries[j].Dir; a != b {
			return a // dirs first
		}
		return strings.ToLower(out.Entries[i].Name) < strings.ToLower(out.Entries[j].Name)
	})
	return out, nil
}

// Write atomically writes data at rel (temp file + rename), creating parent
// directories as needed.
func (fs *FileService) Write(rel string, data []byte) (*FileInfo, error) {
	if len(data) > MaxFileBytes {
		return nil, ErrTooLarge
	}
	abs, err := fs.resolve(rel, true)
	if err != nil {
		return nil, err
	}
	if abs == fs.root {
		return nil, ErrIsDir
	}
	dir := filepath.Dir(abs)
	if st, err := os.Stat(abs); err == nil && st.IsDir() {
		return nil, ErrIsDir
	}
	if st, err := os.Lstat(dir); err == nil && !st.IsDir() {
		return nil, ErrNotDir
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(dir, ".ferrous-*")
	if err != nil {
		return nil, err
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op after successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(name, abs); err != nil {
		return nil, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	return &FileInfo{
		Path: cleanRel(rel), Type: "file",
		Size: st.Size(), MTime: st.ModTime().UTC().Format(time.RFC3339),
	}, nil
}

// DecodeContent turns {"content","encoding"} into bytes.
func DecodeContent(content, encoding string) ([]byte, error) {
	switch encoding {
	case "", "utf8":
		return []byte(content), nil
	case "base64":
		b, err := base64.StdEncoding.DecodeString(content)
		if err != nil {
			return nil, fmt.Errorf("invalid base64 content: %w", err)
		}
		return b, nil
	default:
		return nil, fmt.Errorf("unknown encoding %q (want utf8 or base64)", encoding)
	}
}

func encodingOf(b []byte) string {
	if utf8.Valid(b) && !bytes.ContainsRune(b, 0) {
		return "utf8"
	}
	return "base64"
}

func contentOf(b []byte, enc string) string {
	if enc == "base64" {
		return base64.StdEncoding.EncodeToString(b)
	}
	return string(b)
}

// cleanRel normalizes for responses: no leading slash, "" for root.
func cleanRel(rel string) string {
	if rel == "" || rel == "/" {
		return ""
	}
	return strings.TrimPrefix(filepath.Clean("/"+rel), "/")
}
