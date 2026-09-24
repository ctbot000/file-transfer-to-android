package server

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ctbot000/file-transfer-to-android/internal/qrcode"
	"github.com/ctbot000/file-transfer-to-android/web"
)

func (s *Server) desktopRoutes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { servePage(w, "desktop.html") })
	api := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.requireKey(h)) }
	api("GET /api/ping", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	api("GET /api/state", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.desktopState()) })
	api("GET /api/events", s.desktopEvents)
	api("POST /api/items", s.addItems)
	api("DELETE /api/items/{id}", s.removeItem)
	api("PUT /api/stage/{stage}/{path...}", s.stageFile)
	api("POST /api/stage/{stage}/share", s.shareStage)
	api("DELETE /api/stage/{stage}", s.discardStage)
	api("POST /api/reveal", s.reveal)
	api("POST /api/quit", s.quit)
	return mux
}

const pageCSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

func servePage(w http.ResponseWriter, name string) {
	b, err := web.FS.ReadFile(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", pageCSP)
	w.Write(b)
}

// requireKey admits requests that carry the admin key: in the X-Admin-Key
// header, or for GET requests (which EventSource cannot add headers to) in
// the "key" query parameter. Other sites cannot send the header without a
// CORS preflight, which is never granted, so this also stops CSRF.
func (s *Server) requireKey(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-Admin-Key")
		if key == "" && r.Method == http.MethodGet {
			key = r.URL.Query().Get("key")
		}
		if s.cfg.AdminKey == "" || subtle.ConstantTimeCompare([]byte(key), []byte(s.cfg.AdminKey)) != 1 {
			http.Error(w, "Open this page with the link printed in the terminal.", http.StatusUnauthorized)
			return
		}
		h(w, r)
	})
}

type desktopItem struct {
	itemJSON
	Path    string `json:"path"`
	Staged  bool   `json:"staged"`
	Missing bool   `json:"missing"`
}

type linkJSON struct {
	URL string `json:"url"`
	QR  string `json:"qr"` // SVG markup
}

type desktopStateJSON struct {
	Name    string        `json:"name"`
	Version string        `json:"version"`
	Links   []linkJSON    `json:"links"`
	Items   []desktopItem `json:"items"`
	Receive struct {
		Enabled bool   `json:"enabled"`
		Dir     string `json:"dir"`  // for display
		Path    string `json:"path"` // for "Show in Finder"
	} `json:"receive"`
	Received  []receivedFile `json:"received"`
	Phones    []string       `json:"phones"`
	Transfers []transferJSON `json:"transfers"`
}

func (s *Server) desktopState() desktopStateJSON {
	st := desktopStateJSON{
		Name:      s.cfg.Name,
		Version:   s.cfg.Version,
		Links:     []linkJSON{},
		Items:     []desktopItem{},
		Received:  s.received.list(),
		Phones:    s.phones.list(),
		Transfers: s.transfers.snapshot(),
	}
	for _, u := range s.cfg.PhoneURLs() {
		st.Links = append(st.Links, linkJSON{URL: u, QR: s.qrSVG(u)})
	}
	for _, it := range s.items.list() {
		j, ok := describe(it)
		st.Items = append(st.Items, desktopItem{itemJSON: j, Path: it.Path, Staged: it.Staged != "", Missing: !ok})
	}
	if s.cfg.ReceiveDir != "" {
		st.Receive.Enabled = true
		st.Receive.Dir = DisplayPath(s.cfg.ReceiveDir)
		st.Receive.Path = s.cfg.ReceiveDir
	}
	return st
}

func (s *Server) qrSVG(url string) string {
	s.qr.mu.Lock()
	defer s.qr.mu.Unlock()
	if svg, ok := s.qr.svg[url]; ok {
		return svg
	}
	code, err := qrcode.Encode(url)
	if err != nil {
		return ""
	}
	if s.qr.svg == nil {
		s.qr.svg = make(map[string]string)
	}
	svg := code.SVG(4)
	s.qr.svg[url] = svg
	return svg
}

func (s *Server) desktopEvents(w http.ResponseWriter, r *http.Request) {
	sub := s.hub.subscribe(desktopPages)
	defer s.hub.unsubscribe(sub)
	s.stream(w, r, sub, []string{eventState}, func(event string) any {
		switch event {
		case eventState:
			return s.desktopState()
		case eventTransfers:
			return s.transfers.snapshot()
		}
		return nil
	})
}

// addItems shares files and folders by path. A second run of the app uses
// it to add to this one instead of starting another server.
func (s *Server) addItems(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Paths []string `json:"paths"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	type result struct {
		Path  string `json:"path"`
		Added bool   `json:"added"`
		Error string `json:"error,omitempty"`
	}
	results := []result{}
	changed := false
	for _, p := range req.Paths {
		res := result{Path: p}
		if !filepath.IsAbs(p) {
			res.Error = "not an absolute path"
		} else if _, added, err := s.items.add(p, ""); err != nil {
			res.Error = err.Error()
		} else {
			res.Added = added
			changed = changed || added
		}
		results = append(results, res)
	}
	if changed {
		s.itemsChanged()
	}
	writeJSON(w, results)
}

func (s *Server) removeItem(w http.ResponseWriter, r *http.Request) {
	it, ok := s.items.remove(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if it.Staged != "" {
		s.removeStage(it.Staged)
	}
	s.itemsChanged()
	w.WriteHeader(http.StatusNoContent)
}

// A stage is a directory under StagingDir holding one file or folder
// dropped on the desktop page while it is copied in. The page names it
// with a random ID, uploads its files one request each, then shares it.
var stageID = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

func (s *Server) stageDir(id string) (string, bool) {
	if s.cfg.StagingDir == "" || !stageID.MatchString(id) {
		return "", false
	}
	return filepath.Join(s.cfg.StagingDir, id), true
}

// removeStage deletes a stage directory, refusing anything that is not
// directly inside StagingDir.
func (s *Server) removeStage(dir string) {
	if s.cfg.StagingDir == "" || filepath.Dir(dir) != filepath.Clean(s.cfg.StagingDir) {
		return
	}
	_ = os.RemoveAll(dir)
}

func (s *Server) stageFile(w http.ResponseWriter, r *http.Request) {
	dir, ok := s.stageDir(r.PathValue("stage"))
	rel := r.PathValue("path")
	if !ok || !fs.ValidPath(rel) || rel == "." || strings.Contains(rel, `\`) {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer root.Close()
	name := filepath.FromSlash(rel)
	if parent := path.Dir(rel); parent != "." {
		if err := root.MkdirAll(filepath.FromSlash(parent), 0o700); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, err = io.Copy(f, r.Body)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = root.Remove(name)
		http.Error(w, "copy failed", http.StatusInternalServerError)
		return
	}
	if ms, err := strconv.ParseInt(r.URL.Query().Get("modified"), 10, 64); err == nil && ms > 0 {
		_ = root.Chtimes(name, time.Time{}, time.UnixMilli(ms))
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) shareStage(w http.ResponseWriter, r *http.Request) {
	dir, ok := s.stageDir(r.PathValue("stage"))
	var req struct {
		Name string `json:"name"`
		Dir  bool   `json:"dir"`
	}
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req)
	if !ok || err != nil || !fs.ValidPath(req.Name) || req.Name == "." || strings.ContainsAny(req.Name, `/\`) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	p := filepath.Join(dir, req.Name)
	if req.Dir {
		// An empty folder has no files whose upload would have created it.
		if err := os.MkdirAll(p, 0o700); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	it, _, err := s.items.add(p, dir)
	if err != nil {
		http.Error(w, "nothing to share", http.StatusBadRequest)
		return
	}
	s.itemsChanged()
	writeJSON(w, struct {
		ID string `json:"id"`
	}{it.ID})
}

func (s *Server) discardStage(w http.ResponseWriter, r *http.Request) {
	dir, ok := s.stageDir(r.PathValue("stage"))
	if !ok {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if s.items.usesStage(dir) {
		http.Error(w, "already shared", http.StatusConflict)
		return
	}
	s.removeStage(dir)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) quit(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Quit == nil {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusNoContent)
	s.cfg.Logf("Stopped from the browser")
	s.cfg.Quit()
}

// reveal shows a received file, or the receive folder, in the system file
// manager. Other paths are refused.
func (s *Server) reveal(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	allowed := req.Path != "" && (req.Path == s.cfg.ReceiveDir || s.received.contains(req.Path))
	if !allowed || s.cfg.Reveal == nil {
		http.NotFound(w, r)
		return
	}
	if err := s.cfg.Reveal(req.Path); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
