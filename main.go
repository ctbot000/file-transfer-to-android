// Command file-transfer-to-android sends files from a computer to an
// Android phone over Wi-Fi, with nothing to install on the phone.
//
// It serves the files over HTTP on the local network and shows a QR code.
// The phone scans it and gets a page in its browser that downloads the
// files, and sends files back the other way. The computer's own browser
// gets a page for adding and removing files by drag and drop.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ctbot000/file-transfer-to-android/internal/netaddr"
	"github.com/ctbot000/file-transfer-to-android/internal/qrcode"
	"github.com/ctbot000/file-transfer-to-android/internal/server"
)

const (
	command     = "file-transfer-to-android"
	defaultPort = 8686
)

// version is set at release build time with -ldflags "-X main.version=...".
var version = ""

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

type options struct {
	port       int
	portSet    bool
	host       string
	name       string
	receiveDir string
	noReceive  bool
	noBrowser  bool
	paths      []string
}

func run(args []string, stdout, stderr io.Writer) int {
	opts, err := parseOptions(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		if !errors.Is(err, errReported) {
			fmt.Fprintf(stderr, "%s: %v\n", command, err)
		}
		return 2
	}

	paths := make([]string, 0, len(opts.paths))
	for _, p := range opts.paths {
		abs, err := filepath.Abs(p)
		if err == nil {
			_, err = os.Stat(abs)
		}
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", command, err)
			return 1
		}
		paths = append(paths, abs)
	}

	// Running the command again while it is already open hands the files
	// to the open session instead of starting a second server.
	if s := findSession(); s != nil {
		return s.handOff(paths, opts, stdout, stderr)
	}
	if err := serve(opts, paths, stdout); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", command, err)
		return 1
	}
	return 0
}

// errReported marks a flag error that the flag package already printed.
var errReported = errors.New("reported")

func parseOptions(args []string, stderr io.Writer) (options, error) {
	opts := options{}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.IntVar(&opts.port, "port", defaultPort, "`port` to listen on; 0 picks any free port")
	fs.StringVar(&opts.host, "host", "", "`address` to put in the phone link, instead of the detected one")
	fs.StringVar(&opts.name, "name", "", "`name` of this computer to show on the phone")
	fs.StringVar(&opts.receiveDir, "receive-dir", "", "`folder` where files sent from the phone are saved (default ~/Downloads)")
	fs.BoolVar(&opts.noReceive, "no-receive", false, "do not let the phone send files to this computer")
	fs.BoolVar(&opts.noBrowser, "no-browser", false, "do not open the page for managing shared files")
	showVersion := fs.Bool("version", false, "print the version and exit")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), `Usage: %s [options] [file or folder ...]

Shares files with an Android phone on the same Wi-Fi network. Scan the QR
code with the phone to download the files in its browser, or to send files
back to this computer. Nothing needs to be installed on the phone.

Files can also be added and removed in the page this opens in your browser.
Running the command again while it is open adds files to the open session.

Options:
`, command)
		fs.PrintDefaults()
	}

	// Allow options after file names too: flag stops at the first non-flag
	// argument, so parse again after each one.
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return opts, err
			}
			return opts, errReported
		}
		rest := fs.Args()
		if n := len(args) - len(rest); n > 0 && args[n-1] == "--" {
			opts.paths = append(opts.paths, rest...)
			break
		}
		if len(rest) == 0 {
			break
		}
		opts.paths = append(opts.paths, rest[0])
		args = rest[1:]
	}
	if *showVersion {
		fmt.Fprintln(fs.Output(), command, appVersion())
		return opts, flag.ErrHelp
	}
	fs.Visit(func(f *flag.Flag) { opts.portSet = opts.portSet || f.Name == "port" })
	if opts.port < 0 || opts.port > 65535 {
		return opts, fmt.Errorf("invalid port %d", opts.port)
	}
	return opts, nil
}

func appVersion() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

func defaultReceiveDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Downloads")
}

func serve(opts options, paths []string, stdout io.Writer) error {
	// Stopping comes from Ctrl+C, a termination signal, or the desktop
	// page's Stop button.
	quitCtx, quit := context.WithCancel(context.Background())
	defer quit()
	ctx, stop := signal.NotifyContext(quitCtx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	receiveDir := ""
	if !opts.noReceive {
		if opts.receiveDir == "" {
			opts.receiveDir = defaultReceiveDir()
		}
		if opts.receiveDir == "" {
			return errors.New("no folder to save received files in; use -receive-dir or -no-receive")
		}
		abs, err := filepath.Abs(opts.receiveDir)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return err
		}
		receiveDir = abs
	}
	name := opts.name
	if name == "" {
		name = computerName()
	}

	staging, err := os.MkdirTemp("", command+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)

	ln, err := net.Listen("tcp", ":"+strconv.Itoa(opts.port))
	if err != nil && !opts.portSet {
		ln, err = net.Listen("tcp", ":0")
	}
	if err != nil {
		return err
	}
	port := ln.Addr().(*net.TCPAddr).Port

	adminKey := randomToken(26)
	links := &phoneLinks{host: opts.host, port: port, token: randomToken(12)}
	links.refresh()
	logger := log.New(stdout, "  ", log.Ltime)

	srv := server.New(server.Config{
		Name:       name,
		Version:    appVersion(),
		Token:      links.token,
		AdminKey:   adminKey,
		ReceiveDir: receiveDir,
		StagingDir: staging,
		PhoneURLs:  links.get,
		Reveal:     reveal,
		Quit:       quit,
		Logf:       logger.Printf,
	})
	defer srv.Close()
	var shared []string
	for _, p := range paths {
		if _, err := srv.AddPath(p); err != nil {
			return err
		}
		shared = append(shared, filepath.Base(p))
	}

	httpServer := &http.Server{
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	served := make(chan error, 1)
	go func() { served <- httpServer.Serve(ln) }()

	session := saveSession(port, adminKey)
	defer session.remove()

	desktopURL := fmt.Sprintf("http://localhost:%d/#k=%s", port, adminKey)
	printBanner(stdout, bannerInfo{
		links:      links.get(),
		desktopURL: desktopURL,
		shared:     shared,
		receiveDir: receiveDir,
	})
	if !opts.noBrowser {
		if err := openURL(desktopURL); err != nil {
			logger.Printf("Could not open a browser: %v", err)
		}
	}

	if opts.host == "" {
		go links.watch(ctx, func(urls []string) {
			if len(urls) == 0 {
				logger.Printf("Network connection lost. Waiting for one…")
			} else {
				logger.Printf("Network changed. Phone link is now %s", urls[0])
				printQR(stdout, urls[0])
			}
			srv.NetworkChanged()
		})
	}

	select {
	case <-ctx.Done():
	case err := <-served:
		return err
	}
	stop() // a second Ctrl+C now exits immediately
	fmt.Fprintln(stdout, "\n  Stopping…")
	srv.Close()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		httpServer.Close()
	}
	return nil
}

// randomToken returns n random characters of lowercase base32: 5 bits each.
func randomToken(n int) string {
	b := make([]byte, (n*5+7)/8)
	rand.Read(b)
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))[:n]
}

// phoneLinks holds the URLs a phone can open, following network changes.
type phoneLinks struct {
	host  string // fixed by -host, or empty to detect
	port  int
	token string

	mu   sync.Mutex
	urls []string
}

func (l *phoneLinks) url(host string) string {
	return "http://" + net.JoinHostPort(host, strconv.Itoa(l.port)) + "/" + l.token + "/"
}

func (l *phoneLinks) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.urls)
}

// refresh recomputes the URLs and reports whether they changed.
func (l *phoneLinks) refresh() bool {
	var urls []string
	if l.host != "" {
		urls = []string{l.url(l.host)}
	} else {
		for _, c := range netaddr.Candidates() {
			urls = append(urls, l.url(c.IP.String()))
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if slices.Equal(urls, l.urls) {
		return false
	}
	l.urls = urls
	return true
}

func (l *phoneLinks) watch(ctx context.Context, changed func([]string)) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if l.refresh() {
				changed(l.get())
			}
		}
	}
}

type bannerInfo struct {
	links      []string
	desktopURL string
	shared     []string
	receiveDir string
}

func printBanner(w io.Writer, b bannerInfo) {
	fmt.Fprintf(w, "\n  File Transfer to Android %s\n\n", appVersion())
	if len(b.links) == 0 {
		fmt.Fprintf(w, "  No network connection found. Connect this computer to Wi-Fi;\n  the phone link appears here when it is ready.\n")
	} else {
		printQR(w, b.links[0])
		fmt.Fprintf(w, "  Scan the QR code with your Android phone's camera, or open:\n    %s\n", b.links[0])
		if len(b.links) > 1 {
			fmt.Fprintf(w, "  If that does not load, try:\n")
			for _, u := range b.links[1:] {
				fmt.Fprintf(w, "    %s\n", u)
			}
		}
	}
	fmt.Fprintf(w, "\n  Add or remove files: %s\n", b.desktopURL)
	if len(b.shared) > 0 {
		fmt.Fprintf(w, "  Sharing: %s\n", strings.Join(b.shared, ", "))
	}
	if b.receiveDir != "" {
		fmt.Fprintf(w, "  Files from the phone are saved to %s\n", server.DisplayPath(b.receiveDir))
	}
	fmt.Fprintf(w, "  Press Ctrl+C to stop.\n\n")
}

// printQR draws the link as a QR code when the output is a terminal.
func printQR(w io.Writer, url string) {
	if f, ok := w.(*os.File); !ok || !isTerminal(f) {
		return
	}
	code, err := qrcode.Encode(url)
	if err != nil {
		return
	}
	code.WriteTerminal(w, 2, "  ")
	fmt.Fprintln(w)
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// session describes the running instance, so that running the command
// again can hand its files over. The admin key makes the file a secret; it
// is only readable by its owner.
type session struct {
	PID  int    `json:"pid"`
	Port int    `json:"port"`
	Key  string `json:"key"`
	path string
}

func sessionPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, command, "session.json"), nil
}

// findSession returns the running instance, or nil if there is none.
func findSession() *session {
	p, err := sessionPath()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	s := &session{path: p}
	if json.Unmarshal(data, s) != nil || s.Port == 0 || s.Key == "" {
		return nil
	}
	resp, err := s.request(http.MethodGet, "/api/ping", nil)
	if err != nil {
		return nil
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return nil
	}
	return s
}

func (s *session) request(method, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", s.Port, path), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Admin-Key", s.Key)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Second}
	return client.Do(req)
}

func (s *session) desktopURL() string {
	return fmt.Sprintf("http://localhost:%d/#k=%s", s.Port, s.Key)
}

// handOff adds paths to the running instance, or opens its page when there
// are none.
func (s *session) handOff(paths []string, opts options, stdout, stderr io.Writer) int {
	if len(paths) == 0 {
		fmt.Fprintf(stdout, "Already running: %s\n", s.desktopURL())
		if !opts.noBrowser {
			if err := openURL(s.desktopURL()); err != nil {
				fmt.Fprintf(stderr, "%s: could not open a browser: %v\n", command, err)
			}
		}
		return 0
	}
	body, _ := json.Marshal(map[string][]string{"paths": paths})
	resp, err := s.request(http.MethodPost, "/api/items", body)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", command, err)
		return 1
	}
	defer resp.Body.Close()
	var results []struct {
		Path  string `json:"path"`
		Added bool   `json:"added"`
		Error string `json:"error"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&results) != nil {
		fmt.Fprintf(stderr, "%s: the running session did not accept the files (%s)\n", command, resp.Status)
		return 1
	}
	status := 0
	for _, r := range results {
		switch {
		case r.Error != "":
			fmt.Fprintf(stderr, "%s: %s: %s\n", command, r.Path, r.Error)
			status = 1
		case r.Added:
			fmt.Fprintf(stdout, "Now sharing %s\n", filepath.Base(r.Path))
		default:
			fmt.Fprintf(stdout, "Already sharing %s\n", filepath.Base(r.Path))
		}
	}
	return status
}

func saveSession(port int, key string) *session {
	s := &session{PID: os.Getpid(), Port: port, Key: key}
	p, err := sessionPath()
	if err != nil {
		return s
	}
	s.path = p
	data, _ := json.Marshal(s)
	if os.MkdirAll(filepath.Dir(p), 0o700) == nil {
		_ = os.WriteFile(p, data, 0o600)
	}
	return s
}

// remove deletes the session file, unless another instance has replaced it.
func (s *session) remove() {
	if s.path == "" {
		return
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var current session
	if json.Unmarshal(data, &current) == nil && current.Key == s.Key {
		os.Remove(s.path)
	}
}
