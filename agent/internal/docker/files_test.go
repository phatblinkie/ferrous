package docker

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- DataRoot ---------------------------------------------------------------

func TestDataRootLabelWins(t *testing.T) {
	d := &ContainerDetail{}
	d.Config.Labels = map[string]string{"ferrous.datadir": "/srv/ferrous/x/"}
	d.Mounts = []Mount{{Type: "bind", Source: "/other", Destination: "/server", RW: true}}
	root, err := DataRoot(d)
	if err != nil || root != "/srv/ferrous/x" {
		t.Fatalf("got %q, %v", root, err)
	}
}

func TestDataRootLabelValidation(t *testing.T) {
	for _, tc := range []struct{ label, wantErr string }{
		{"relative/path", "must be absolute"},
		{"/", "refuses /"},
	} {
		d := &ContainerDetail{}
		d.Config.Labels = map[string]string{"ferrous.datadir": tc.label}
		if _, err := DataRoot(d); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("label %q: want err containing %q, got %v", tc.label, tc.wantErr, err)
		}
	}
}

func TestDataRootMountFallbacks(t *testing.T) {
	// exactly one RW mount → it
	d := &ContainerDetail{}
	d.Mounts = []Mount{
		{Source: "/data-ro", Destination: "/ro", RW: false},
		{Source: "/srv/x", Destination: "/server", RW: true},
	}
	root, err := DataRoot(d)
	if err != nil || root != "/srv/x" {
		t.Fatalf("single RW mount: got %q, %v", root, err)
	}
	// ambiguous → error mentioning the label
	d.Mounts = []Mount{{Source: "/a", RW: true}, {Source: "/b", RW: true}}
	if _, err := DataRoot(d); err == nil || !strings.Contains(err.Error(), "ferrous.datadir") {
		t.Fatalf("ambiguous mounts: want label hint, got %v", err)
	}
	// none → error
	if _, err := DataRoot(&ContainerDetail{}); err == nil {
		t.Fatal("no mounts: want error")
	}
}

// --- NewFileService ---------------------------------------------------------

func TestNewFileService(t *testing.T) {
	root := t.TempDir()
	svc, err := NewFileService(root)
	if err != nil {
		t.Fatal(err)
	}
	if svc.Root() == "" || !strings.HasPrefix(svc.Root(), root) {
		t.Fatalf("root not resolved: %q", svc.Root())
	}
	if _, err := NewFileService(filepath.Join(root, "missing")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing root: want ErrNotFound, got %v", err)
	}
	if _, err := NewFileService("/"); !errors.Is(err, ErrNotDir) {
		t.Fatalf("/ root: want ErrNotDir, got %v", err)
	}
}

// --- resolve: the security core --------------------------------------------

func TestResolveRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	svc, _ := NewFileService(root)

	for _, rel := range []string{
		"..", "../", "../etc/passwd", "a/../../etc", "a/b/../../../x",
	} {
		if _, err := svc.resolve(rel, false); !errors.Is(err, ErrEscape) {
			t.Errorf("resolve(%q): want ErrEscape, got %v", rel, err)
		}
		if _, err := svc.resolve(rel, true); !errors.Is(err, ErrEscape) {
			t.Errorf("resolve(%q, missingOK): want ErrEscape, got %v", rel, err)
		}
	}
	for _, rel := range []string{"/etc/passwd", `/windows`} {
		if _, err := svc.resolve(rel, false); err == nil || errors.Is(err, ErrEscape) {
			t.Errorf("resolve(%q): want relative-path error, got %v", rel, err)
		}
	}
}

func TestResolveNormalizes(t *testing.T) {
	root := t.TempDir()
	svc, _ := NewFileService(root)
	for _, rel := range []string{"", "/", ".", "./", "a//b", "a/./b"} {
		got, err := svc.resolve(rel, false)
		if err != nil && !errors.Is(err, ErrNotFound) {
			t.Errorf("resolve(%q): unexpected %v", rel, err)
		}
		if got != "" && !strings.HasPrefix(got, svc.Root()) {
			t.Errorf("resolve(%q) escaped: %q", rel, got)
		}
	}
	// clean path under root maps exactly (after normalization)
	os.MkdirAll(filepath.Join(root, "server/main"), 0o755)
	os.WriteFile(filepath.Join(root, "server/main/server.cfg"), []byte("x"), 0o644)
	full := filepath.Join(svc.Root(), "server/main/server.cfg")
	if got, err := svc.resolve("server/main/server.cfg", false); err != nil || got != full {
		t.Fatalf("clean resolve: got %q, %v", got, err)
	}
	if got, err := svc.resolve("./server//main/./server.cfg", false); err != nil || got != full {
		t.Fatalf("normalized resolve: got %q, %v", got, err)
	}
}

func TestSymlinkEscapeRejected(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// file symlink leaving root
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "link.cfg")); err != nil {
		t.Fatal(err)
	}
	// dir symlink leaving root
	if err := os.Symlink(outside, filepath.Join(root, "ldir")); err != nil {
		t.Fatal(err)
	}
	// safe symlink INSIDE root
	if err := os.MkdirAll(filepath.Join(root, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "inside")); err != nil {
		t.Fatal(err)
	}

	svc, _ := NewFileService(root)
	if _, err := svc.Get("link.cfg"); !errors.Is(err, ErrEscape) {
		t.Errorf("file symlink: want ErrEscape, got %v", err)
	}
	if _, err := svc.Get("ldir/secret"); !errors.Is(err, ErrEscape) {
		t.Errorf("dir symlink: want ErrEscape, got %v", err)
	}
	if _, err := svc.Write("link.cfg", []byte("clobber")); !errors.Is(err, ErrEscape) {
		t.Errorf("write through symlink: want ErrEscape, got %v", err)
	}
	if _, err := svc.Get("inside/ok.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("inside symlink should resolve: got %v", err)
	}
}

// --- Get --------------------------------------------------------------------

func TestGetFileTextAndBinary(t *testing.T) {
	root := t.TempDir()
	svc, _ := NewFileService(root)
	if err := os.WriteFile(filepath.Join(root, "server.cfg"), []byte("server.name \"hi\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := []byte{0x4d, 0x5a, 0x00, 0x01, 0xff}
	if err := os.WriteFile(filepath.Join(root, "plugin.dll"), bin, 0o644); err != nil {
		t.Fatal(err)
	}

	fi, err := svc.Get("server.cfg")
	if err != nil || fi.Type != "file" || fi.Encoding != "utf8" || fi.Content != "server.name \"hi\"\n" {
		t.Fatalf("text file: %+v, %v", fi, err)
	}
	if fi.Size != int64(len("server.name \"hi\"\n")) || fi.MTime == "" {
		t.Errorf("text meta: size=%d mtime=%q", fi.Size, fi.MTime)
	}

	fi, err = svc.Get("plugin.dll")
	if err != nil || fi.Encoding != "base64" {
		t.Fatalf("binary: %+v, %v", fi, err)
	}
	data, err := DecodeContent(fi.Content, "base64")
	if err != nil || !bytes.Equal(data, bin) {
		t.Fatalf("base64 roundtrip: %v, %v", data, err)
	}
}

func TestGetDirListing(t *testing.T) {
	root := t.TempDir()
	svc, _ := NewFileService(root)
	os.MkdirAll(filepath.Join(root, "oxide/plugins"), 0o755)
	os.WriteFile(filepath.Join(root, "aaa.txt"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(root, "oxide", "cfg"), []byte("c"), 0o644)

	fi, err := svc.Get("")
	if err != nil || fi.Type != "dir" {
		t.Fatalf("root list: %+v, %v", fi, err)
	}
	// dirs first: oxide before aaa.txt
	if len(fi.Entries) != 2 || fi.Entries[0].Name != "oxide" || !fi.Entries[0].Dir ||
		fi.Entries[1].Name != "aaa.txt" || fi.Entries[1].Dir || fi.Entries[1].Size != 1 {
		t.Fatalf("listing: %+v", fi.Entries)
	}
	if _, err := svc.Get("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestGetTooLarge(t *testing.T) {
	root := t.TempDir()
	svc, _ := NewFileService(root)
	if err := os.WriteFile(filepath.Join(root, "big"), bytes.Repeat([]byte("x"), MaxFileBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get("big"); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
}

// --- Write ------------------------------------------------------------------

func TestWriteCreatesParentsAndOverwrites(t *testing.T) {
	root := t.TempDir()
	svc, _ := NewFileService(root)

	fi, err := svc.Write("oxide/plugins/New.dll", []byte("MZ-fake"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Path != "oxide/plugins/New.dll" || fi.Size != 7 {
		t.Fatalf("meta: %+v", fi)
	}
	got, err := os.ReadFile(filepath.Join(root, "oxide/plugins/New.dll"))
	if err != nil || string(got) != "MZ-fake" {
		t.Fatalf("written: %q %v", got, err)
	}

	// overwrite is atomic (no partial states visible at the final path)
	if _, err := svc.Write("oxide/plugins/New.dll", []byte("v2")); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(filepath.Join(root, "oxide/plugins/New.dll"))
	if string(got) != "v2" {
		t.Fatalf("overwrite: %q", got)
	}
	// no temp litter
	entries, _ := os.ReadDir(filepath.Join(root, "oxide/plugins"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".ferrous-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestWriteGuards(t *testing.T) {
	root := t.TempDir()
	svc, _ := NewFileService(root)
	os.MkdirAll(filepath.Join(root, "adir"), 0o755)

	if _, err := svc.Write("", []byte("x")); !errors.Is(err, ErrIsDir) {
		t.Errorf("write root: %v", err)
	}
	if _, err := svc.Write("adir", []byte("x")); !errors.Is(err, ErrIsDir) {
		t.Errorf("write dir: %v", err)
	}
	os.WriteFile(filepath.Join(root, "afile"), []byte("x"), 0o644)
	if _, err := svc.Write("afile/child", []byte("x")); !errors.Is(err, ErrNotDir) {
		t.Errorf("parent is file: %v", err)
	}
	if _, err := svc.Write("../escape", []byte("x")); !errors.Is(err, ErrEscape) {
		t.Errorf("write escape: %v", err)
	}
	if _, err := svc.Write("big", bytes.Repeat([]byte("x"), MaxFileBytes+1)); !errors.Is(err, ErrTooLarge) {
		t.Errorf("write too large: %v", err)
	}
}

func TestDecodeContent(t *testing.T) {
	if b, err := DecodeContent("hi", ""); err != nil || string(b) != "hi" {
		t.Errorf("default: %q %v", b, err)
	}
	if b, err := DecodeContent("aGk=", "base64"); err != nil || string(b) != "hi" {
		t.Errorf("base64: %q %v", b, err)
	}
	if _, err := DecodeContent("!!", "base64"); err == nil {
		t.Error("bad base64: want error")
	}
	if _, err := DecodeContent("x", "rot13"); err == nil {
		t.Error("unknown encoding: want error")
	}
}
