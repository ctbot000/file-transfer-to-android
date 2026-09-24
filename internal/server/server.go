// Package server is the HTTP side of File Transfer to Android.
//
// It serves two pages. The phone page lives under a secret path segment
// (the token in the QR code); it lists the shared files, downloads them,
// and sends files back to the computer. The desktop page is only served to
// this computer's own browser; it shows the QR code and manages what is
// shared.
package server

import (
	"crypto/subtle"
	"encoding/json"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/ctbot000/file-transfer-to-android/web"
)

// Config configures a Server.
type Config struct {
	// Name identifies this computer on the phone page.
	Name string
	// Version is shown on the desktop page.
	Version string
	// Token is the secret first path segment of every phone URL.
	Token string
	// AdminKey authorizes the desktop page's API requests.
	AdminKey string
	// ReceiveDir is where files sent from the phone are saved. Empty turns
	// receiving off.
	ReceiveDir string
	// StagingDir holds the copies of files added through the desktop page.
	StagingDir string
	// PhoneURLs returns the URLs a phone can open, best first.
	PhoneURLs func() []string
	// Reveal shows a file or folder in the system file manager.
	Reveal func(path string) error
	// Quit stops the app. The desktop page's Stop button calls it.
	Quit func()
	// Logf prints one line per notable event, such as a finished transfer.
	Logf func(format string, args ...any)
}

// Server serves the phone and desktop pages. It is an http.Handler.
type Server struct {
	cfg       Config
	items     registry
	hub       *hub
	transfers transfers
	received  receivedLog
	phones    phoneTracker
	qr        qrCache

	phoneMux   *http.ServeMux
	desktopMux *http.ServeMux
	static     http.Handler

	done      chan struct{}
	closeOnce sync.Once
}

// New returns a Server for cfg. Call Close when done with it.
func New(cfg Config) *Server {
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	if cfg.PhoneURLs == nil {
		cfg.PhoneURLs = func() []string { return nil }
	}
	s := &Server{
		cfg:  cfg,
		hub:  newHub(),
		done: make(chan struct{}),
	}
	s.phones.seen = make(map[string]int)
	s.phones.announced = make(map[string]bool)

	assets, err := fs.Sub(web.FS, "static")
	if err != nil {
		panic(err)
	}
	s.static = http.StripPrefix("/static/", http.FileServerFS(assets))
	s.phoneMux = s.phoneRoutes()
	s.desktopMux = s.desktopRoutes()
	go s.publishProgress()
	return s
}

// Close ends the open event streams, so that http.Server.Shutdown does not
// wait for them, and stops background work.
func (s *Server) Close() {
	s.closeOnce.Do(func() { close(s.done) })
}

// AddPath shares a file or folder on this computer. It reports whether
// the path was newly added; sharing a path twice is not an error.
func (s *Server) AddPath(path string) (added bool, err error) {
	_, added, err = s.items.add(path, "")
	if added {
		s.itemsChanged()
	}
	return added, err
}

// NetworkChanged tells open desktop pages to refresh the phone links.
func (s *Server) NetworkChanged() {
	s.hub.publish(desktopPages, eventState)
}

func (s *Server) itemsChanged() {
	s.hub.publish(desktopPages, eventState)
	s.hub.publish(phonePages, eventChanged)
}

// publishProgress sends transfer progress to desktop pages at most a few
// times a second, however many bytes are moving.
func (s *Server) publishProgress() {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-tick.C:
			if s.transfers.changed.Swap(false) {
				s.hub.publish(desktopPages, eventTransfers)
			}
		}
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	// The phone URL carries the token, so it must never leak as a Referer.
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cache-Control", "no-store")

	first, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	switch {
	case first == "static":
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r) // no directory listings
			return
		}
		s.static.ServeHTTP(w, r)
	case first == "" || first == "api":
		if !fromThisComputer(r) {
			s.notFound(w, r)
			return
		}
		s.desktopMux.ServeHTTP(w, r)
	case s.cfg.Token != "" && subtle.ConstantTimeCompare([]byte(first), []byte(s.cfg.Token)) == 1:
		if r.URL.Path == "/"+first {
			http.Redirect(w, r, "/"+first+"/", http.StatusFound)
			return
		}
		http.StripPrefix("/"+first, s.phoneMux).ServeHTTP(w, r)
	default:
		s.notFound(w, r)
	}
}

func (s *Server) notFound(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "Not found. On your phone, open the link shown on the computer; it includes a code.", http.StatusNotFound)
}

// fromThisComputer accepts requests that come over loopback and name a
// loopback host. The Host check defeats DNS rebinding, where a web page's
// own domain is made to resolve to 127.0.0.1.
func fromThisComputer(r *http.Request) bool {
	if !peerIP(r).IsLoopback() {
		return false
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	switch strings.Trim(host, "[]") {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

func peerIP(r *http.Request) netip.Addr {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	return ap.Addr().Unmap()
}

// peer names the other end of a request for logs and the activity list.
func peer(r *http.Request) string {
	if ip := peerIP(r); ip.IsValid() {
		return ip.String()
	}
	return r.RemoteAddr
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// qrCache keeps the rendered QR code of each phone URL; the URLs only
// change when the network does.
type qrCache struct {
	mu  sync.Mutex
	svg map[string]string
}

// phoneTracker counts open phone pages per address, so the desktop page can
// say a phone is connected.
type phoneTracker struct {
	mu        sync.Mutex
	seen      map[string]int
	announced map[string]bool
}

// connect records an open page and reports whether this address has not
// been seen before in this session.
func (p *phoneTracker) connect(addr string) (first bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen[addr]++
	first = !p.announced[addr]
	p.announced[addr] = true
	return first
}

func (p *phoneTracker) disconnect(addr string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.seen[addr]--; p.seen[addr] <= 0 {
		delete(p.seen, addr)
	}
}

func (p *phoneTracker) list() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.seen))
	for addr := range p.seen {
		out = append(out, addr)
	}
	return out
}

// receivedFile is a file the phone sent to this computer.
type receivedFile struct {
	Name string    `json:"name"`
	Path string    `json:"path"`
	Size int64     `json:"size"`
	From string    `json:"from"`
	At   time.Time `json:"at"`
}

type receivedLog struct {
	mu    sync.Mutex
	files []receivedFile // newest first
}

const keepReceived = 100

func (l *receivedLog) add(f receivedFile) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.files = append([]receivedFile{f}, l.files...)
	if len(l.files) > keepReceived {
		l.files = l.files[:keepReceived]
	}
}

func (l *receivedLog) list() []receivedFile {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]receivedFile(nil), l.files...)
}

func (l *receivedLog) contains(path string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, f := range l.files {
		if f.Path == path {
			return true
		}
	}
	return false
}
