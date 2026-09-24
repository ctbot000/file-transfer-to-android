package server

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"time"
)

func (s *Server) phoneRoutes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { servePage(w, "phone.html") })
	mux.HandleFunc("GET /api/items", s.phoneIndex)
	mux.HandleFunc("GET /api/items/{id}/list", s.phoneList)
	mux.HandleFunc("GET /api/events", s.phoneEvents)
	mux.HandleFunc("GET /f/{id}/{path...}", s.download)
	mux.HandleFunc("GET /z/all", s.downloadAll)
	mux.HandleFunc("GET /z/{id}/{path...}", s.downloadFolder)
	mux.HandleFunc("POST /upload", s.upload)
	return mux
}

// itemJSON describes a shared item to either page.
type itemJSON struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Dir      bool      `json:"dir"`
	Size     int64     `json:"size"`
	Files    int       `json:"files"`
	Modified time.Time `json:"modified"`
}

// describe reports an item's current size and, for a folder, how many
// files it would send. ok is false when the item is gone from disk.
func describe(it *item) (j itemJSON, ok bool) {
	j = itemJSON{ID: it.ID, Name: it.Name, Dir: it.Dir}
	info, err := os.Stat(it.Path)
	if err != nil || info.IsDir() != it.Dir {
		return j, false
	}
	j.Modified = info.ModTime()
	if it.Dir {
		stats := it.folderStats()
		j.Size, j.Files = stats.Size, stats.Files
	} else {
		j.Size, j.Files = info.Size(), 1
	}
	return j, true
}

func (s *Server) phoneIndex(w http.ResponseWriter, r *http.Request) {
	items := []itemJSON{}
	for _, it := range s.items.list() {
		if j, ok := describe(it); ok {
			items = append(items, j)
		}
	}
	writeJSON(w, struct {
		Name    string     `json:"name"`
		Receive bool       `json:"receive"`
		Items   []itemJSON `json:"items"`
	}{s.cfg.Name, s.cfg.ReceiveDir != "", items})
}

type entryJSON struct {
	Name     string    `json:"name"`
	Dir      bool      `json:"dir"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

// cleanRel validates a slash-separated path inside a shared folder, as
// sent by the phone. "" means the folder itself. Hidden entries are not
// listed, so they are not served either.
func cleanRel(rel string) (string, bool) {
	if rel == "" {
		return ".", true
	}
	return rel, fs.ValidPath(rel) && !hiddenPath(rel)
}

func (s *Server) phoneList(w http.ResponseWriter, r *http.Request) {
	it, ok := s.items.get(r.PathValue("id"))
	rel, valid := cleanRel(r.URL.Query().Get("path"))
	if !ok || !it.Dir || !valid {
		http.NotFound(w, r)
		return
	}
	root, err := os.OpenRoot(it.Path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer root.Close()
	fsys := root.FS()
	dirEntries, err := fs.ReadDir(fsys, rel)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	entries := []entryJSON{}
	for _, d := range dirEntries {
		if hidden(d.Name()) {
			continue
		}
		if d.IsDir() {
			var mod time.Time
			if info, err := d.Info(); err == nil {
				mod = info.ModTime()
			}
			entries = append(entries, entryJSON{Name: d.Name(), Dir: true, Modified: mod})
			continue
		}
		info, err := fileInfo(fsys, path.Join(rel, d.Name()), d)
		if err != nil || info == nil {
			continue
		}
		entries = append(entries, entryJSON{Name: d.Name(), Size: info.Size(), Modified: info.ModTime()})
	}
	name := it.Name
	if rel != "." {
		name = path.Base(rel)
	}
	if rel == "." {
		rel = ""
	}
	writeJSON(w, struct {
		Item    string      `json:"item"`
		Name    string      `json:"name"`
		Path    string      `json:"path"`
		Entries []entryJSON `json:"entries"`
	}{it.ID, name, rel, entries})
}

func (s *Server) phoneEvents(w http.ResponseWriter, r *http.Request) {
	// The desktop's own browser opening the phone link is not a phone.
	if ip := peerIP(r); !ip.IsLoopback() {
		addr := ip.String()
		if s.phones.connect(addr) {
			s.cfg.Logf("Phone connected from %s", addr)
		}
		s.hub.publish(desktopPages, eventState)
		defer func() {
			s.phones.disconnect(addr)
			s.hub.publish(desktopPages, eventState)
		}()
	}
	sub := s.hub.subscribe(phonePages)
	defer s.hub.unsubscribe(sub)
	s.stream(w, r, sub, nil, func(string) any { return nil })
}

// download serves one file: a shared file, or a file inside a shared
// folder. For a shared file the path segment is only there so the URL ends
// in the file name, which some download managers use as the default name.
func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	it, ok := s.items.get(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	rel := r.PathValue("path")
	var f *os.File
	var name string
	var err error
	if it.Dir {
		if rel, ok = cleanRel(rel); !ok || rel == "." {
			http.NotFound(w, r)
			return
		}
		var root *os.Root
		if root, err = os.OpenRoot(it.Path); err == nil {
			f, err = root.Open(filepath.FromSlash(rel))
			root.Close()
		}
		name = path.Base(rel)
	} else {
		f, err = os.Open(it.Path)
		name = it.Name
	}
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}

	h := w.Header()
	h.Set("Content-Type", contentType(name))
	h.Set("Content-Disposition", contentDisposition("attachment", name))
	// Should a browser ever render a shared file instead of saving it, it
	// must not run with this origin's access to the phone API.
	h.Set("Content-Security-Policy", "sandbox")
	from := peer(r)
	tr := &trackedResponse{
		ResponseWriter: w,
		ts:             &s.transfers,
		get:            r.Method == http.MethodGet,
		key:            from + "\x00" + it.ID + "\x00" + rel,
		name:           name,
		peer:           from,
		size:           info.Size(),
	}
	http.ServeContent(tr, r, name, info.ModTime(), f)
	if done := tr.end(); done != nil {
		s.logSent(*done)
	}
}

// downloadFolder streams a shared folder, or a folder inside one, as a zip
// archive named after it.
func (s *Server) downloadFolder(w http.ResponseWriter, r *http.Request) {
	it, ok := s.items.get(r.PathValue("id"))
	rel, valid := cleanRel(r.PathValue("path"))
	if !ok || !it.Dir || !valid {
		http.NotFound(w, r)
		return
	}
	var z zipBuilder
	defer z.close()
	name, err := z.addItem(it, rel)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.sendZip(w, r, name+".zip", &z)
}

// downloadAll streams every shared item as one zip archive.
func (s *Server) downloadAll(w http.ResponseWriter, r *http.Request) {
	var z zipBuilder
	defer z.close()
	for _, it := range s.items.list() {
		// An item deleted from disk since it was shared is left out.
		_, _ = z.addItem(it, ".")
	}
	if len(z.entries) == 0 {
		http.Error(w, "Nothing is shared right now.", http.StatusNotFound)
		return
	}
	s.sendZip(w, r, "Shared files.zip", &z)
}

// upload saves a file sent from the phone into the receive folder. The
// body is the raw file; the name comes from the query string, along with
// the file's modification time in milliseconds, which is kept.
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	if s.cfg.ReceiveDir == "" {
		http.Error(w, "This computer is not accepting files.", http.StatusForbidden)
		return
	}
	q := r.URL.Query()
	name := sanitizeFilename(q.Get("name"))
	from := peer(r)
	in, err := reserveFile(s.cfg.ReceiveDir, name)
	if err != nil {
		s.cfg.Logf("✗ Could not save %s from %s: %v", name, from, err)
		http.Error(w, "The computer could not save the file.", http.StatusInternalServerError)
		return
	}

	t := s.transfers.begin("", name, fromPhone, from, r.ContentLength, 0)
	body := &countingReader{r: r.Body, t: t}
	n, err := io.Copy(in, body)
	if closeErr := in.Close(); err == nil {
		err = closeErr
	}
	if err == nil && r.ContentLength >= 0 && n != r.ContentLength {
		err = io.ErrUnexpectedEOF
	}
	var final string
	if err == nil {
		final, err = in.commit()
	}
	if err != nil {
		os.Remove(in.part)
		s.transfers.end(t, false)
		if body.err != nil || errors.Is(err, io.ErrUnexpectedEOF) {
			s.cfg.Logf("✗ Receiving %s from %s stopped after %s", name, from, formatSize(n))
			http.Error(w, "The file did not arrive completely. Try again.", http.StatusBadRequest)
		} else {
			s.cfg.Logf("✗ Could not save %s from %s: %v", name, from, err)
			http.Error(w, "The computer could not save the file.", http.StatusInternalServerError)
		}
		return
	}
	if ms, err := strconv.ParseInt(q.Get("modified"), 10, 64); err == nil {
		if mod := time.UnixMilli(ms); mod.Year() >= 1980 && mod.Before(time.Now().Add(24*time.Hour)) {
			_ = os.Chtimes(final, time.Time{}, mod)
		}
	}
	s.transfers.end(t, true)
	saved := filepath.Base(final)
	s.received.add(receivedFile{Name: saved, Path: final, Size: n, From: from, At: time.Now()})
	s.hub.publish(desktopPages, eventState)
	s.cfg.Logf("✓ Received %s (%s) from %s → %s", saved, formatSize(n), from, DisplayPath(final))
	writeJSON(w, struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	}{saved, n})
}

type countingReader struct {
	r   io.Reader
	t   *transfer
	err error // the first read error other than io.EOF
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.t.add(int64(n))
	if err != nil && err != io.EOF && c.err == nil {
		c.err = err
	}
	return n, err
}

func (s *Server) logSent(t transferJSON) {
	if t.State == transferDone {
		s.cfg.Logf("✓ Sent %s (%s) to %s", t.Name, formatSize(t.Bytes), t.Peer)
		return
	}
	if t.Total > 0 {
		s.cfg.Logf("✗ Sending %s to %s stopped at %d%% (%s of %s)", t.Name, t.Peer, t.Bytes*100/t.Total, formatSize(t.Bytes), formatSize(t.Total))
		return
	}
	s.cfg.Logf("✗ Sending %s to %s stopped", t.Name, t.Peer)
}

// formatSize renders a byte count with SI units, as Android and macOS do.
func formatSize(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d B", n)
	}
	v := float64(n)
	for _, unit := range []string{"kB", "MB", "GB", "TB"} {
		v /= 1000
		if v < 999.95 || unit == "TB" {
			if v < 9.995 {
				return fmt.Sprintf("%.2f %s", v, unit)
			}
			if v < 99.95 {
				return fmt.Sprintf("%.1f %s", v, unit)
			}
			return fmt.Sprintf("%.0f %s", v, unit)
		}
	}
	return fmt.Sprintf("%d B", n)
}
