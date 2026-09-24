package server

import (
	"archive/zip"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
)

// zipEntry is one file or folder of an archive being built.
type zipEntry struct {
	name string // slash-separated path inside the archive; folders end in "/"
	mod  time.Time
	size int64
	open func() (fs.File, error) // nil for folders
}

// zipBuilder collects the entries of an archive of shared items, then
// streams it. Nothing is written to disk.
type zipBuilder struct {
	entries []zipEntry
	roots   []*os.Root
	used    map[string]bool // top-level names, kept unique
}

var errNotFolder = errors.New("not a folder")

// addItem adds a shared item to the archive; for a folder item, rel picks
// the subfolder to add ("." for all of it). It returns the item's
// top-level name in the archive, made unique among the items added so far.
func (z *zipBuilder) addItem(it *item, rel string) (string, error) {
	if !it.Dir {
		info, err := os.Stat(it.Path)
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() {
			return "", errNotShareable
		}
		name := z.unique(it.Name)
		z.entries = append(z.entries, zipEntry{
			name: name,
			mod:  info.ModTime(),
			size: info.Size(),
			open: func() (fs.File, error) { return os.Open(it.Path) },
		})
		return name, nil
	}

	root, err := os.OpenRoot(it.Path)
	if err != nil {
		return "", err
	}
	z.roots = append(z.roots, root)
	fsys := root.FS()
	if info, err := fs.Stat(fsys, rel); err != nil {
		return "", err
	} else if !info.IsDir() {
		return "", errNotFolder
	}
	top := it.Name
	if rel != "." {
		top = path.Base(rel)
	}
	top = z.unique(top)
	err = walkFiles(fsys, rel, func(p string, d fs.DirEntry, info fs.FileInfo) error {
		name := top
		if p != rel {
			sub := p
			if rel != "." {
				sub = strings.TrimPrefix(p, rel+"/")
			}
			name += "/" + sub
		}
		if d.IsDir() {
			var mod time.Time
			if di, err := d.Info(); err == nil {
				mod = di.ModTime()
			}
			z.entries = append(z.entries, zipEntry{name: name + "/", mod: mod})
			return nil
		}
		z.entries = append(z.entries, zipEntry{
			name: name,
			mod:  info.ModTime(),
			size: info.Size(),
			open: func() (fs.File, error) { return fsys.Open(p) },
		})
		return nil
	})
	return top, err
}

func (z *zipBuilder) unique(name string) string {
	if z.used == nil {
		z.used = make(map[string]bool)
	}
	candidate := name
	for i := 2; z.used[candidate]; i++ {
		candidate = numbered(name, i)
	}
	z.used[candidate] = true
	return candidate
}

func (z *zipBuilder) size() int64 {
	var total int64
	for _, e := range z.entries {
		total += e.size
	}
	return total
}

// write streams the archive. Files are stored rather than deflated: most
// of what goes to a phone (photos, video, music, APKs) is already
// compressed, and storing keeps the phone's download at full network speed.
func (z *zipBuilder) write(w io.Writer) error {
	zw := zip.NewWriter(w)
	for _, e := range z.entries {
		hdr := &zip.FileHeader{Name: e.name, Method: zip.Store, Modified: e.mod}
		if e.open == nil {
			hdr.SetMode(fs.ModeDir | 0o755)
			if _, err := zw.CreateHeader(hdr); err != nil {
				return err
			}
			continue
		}
		hdr.SetMode(0o644)
		dst, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		src, err := e.open()
		if err != nil {
			return err
		}
		_, err = io.Copy(dst, src)
		src.Close()
		if err != nil {
			return err
		}
	}
	return zw.Close()
}

func (z *zipBuilder) close() {
	for _, r := range z.roots {
		r.Close()
	}
}

// sendZip streams z as a download named filename.
func (s *Server) sendZip(w http.ResponseWriter, r *http.Request, filename string, z *zipBuilder) {
	h := w.Header()
	h.Set("Content-Type", "application/zip")
	h.Set("Content-Disposition", contentDisposition("attachment", filename))
	if r.Method == http.MethodHead {
		return
	}
	t := s.transfers.begin("", filename, toPhone, peer(r), z.size(), 0)
	err := z.write(&countingWriter{w: w, t: t})
	if done := s.transfers.end(t, err == nil); done != nil {
		s.logSent(*done)
	}
	if err != nil {
		// The status line is long gone, so the only way left to report the
		// failure is to cut the response short. Returning normally would end
		// the chunked body cleanly and the phone would keep a broken archive.
		panic(http.ErrAbortHandler)
	}
}

type countingWriter struct {
	w io.Writer
	t *transfer
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.t.add(int64(n))
	return n, err
}
