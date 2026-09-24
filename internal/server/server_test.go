package server

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testToken = "tok3n2345abc"
	testKey   = "adm1nkey"
)

type fixture struct {
	t        *testing.T
	srv      *Server
	http     *httptest.Server
	receive  string
	staging  string
	mu       sync.Mutex
	revealed []string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, receive: t.TempDir(), staging: t.TempDir()}
	f.srv = New(Config{
		Name:       "Test Mac",
		Version:    "test",
		Token:      testToken,
		AdminKey:   testKey,
		ReceiveDir: f.receive,
		StagingDir: f.staging,
		PhoneURLs:  func() []string { return []string{"http://192.168.1.5:8686/" + testToken + "/"} },
		Reveal: func(path string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.revealed = append(f.revealed, path)
			return nil
		},
		Logf: t.Logf,
	})
	f.http = httptest.NewServer(f.srv)
	t.Cleanup(func() {
		f.srv.Close()
		f.http.Close()
	})
	return f
}

// do sends a request to the test server; path is relative to its root.
func (f *fixture) do(method, path string, body io.Reader, header ...string) *http.Response {
	f.t.Helper()
	req, err := http.NewRequest(method, f.http.URL+path, body)
	if err != nil {
		f.t.Fatal(err)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (f *fixture) phone(method, path string, body io.Reader, header ...string) *http.Response {
	f.t.Helper()
	return f.do(method, "/"+testToken+path, body, header...)
}

func (f *fixture) admin(method, path string, body io.Reader) *http.Response {
	f.t.Helper()
	return f.do(method, path, body, "X-Admin-Key", testKey, "Content-Type", "application/json")
}

func (f *fixture) share(path string) {
	f.t.Helper()
	if _, err := f.srv.AddPath(path); err != nil {
		f.t.Fatal(err)
	}
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func decode[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func wantStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		t.Fatalf("%s %s: status %d, want %d", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, want)
	}
}

func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// makeFolder creates a shared folder with a hidden file, a subfolder, an
// empty subfolder, and a symlink that points outside of it.
func makeFolder(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "Trip")
	writeFile(t, filepath.Join(dir, "a.txt"), "alpha")
	writeFile(t, filepath.Join(dir, ".DS_Store"), "junk")
	writeFile(t, filepath.Join(dir, "day 2", "b.txt"), "bravo")
	if err := os.MkdirAll(filepath.Join(dir, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	secret := writeFile(t, filepath.Join(t.TempDir(), "secret.txt"), "secret")
	if err := os.Symlink(secret, filepath.Join(dir, "escape.txt")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	return dir
}

func TestPhonePagesNeedTheToken(t *testing.T) {
	f := newFixture(t)
	wantStatus(t, f.do("GET", "/wrongtoken12/api/items", nil), http.StatusNotFound)
	wantStatus(t, f.do("GET", "/wrongtoken12/", nil), http.StatusNotFound)

	resp := f.do("GET", "/"+testToken, nil)
	wantStatus(t, resp, http.StatusFound)
	if loc := resp.Header.Get("Location"); loc != "/"+testToken+"/" {
		t.Errorf("redirect to %q", loc)
	}

	resp = f.phone("GET", "/", nil)
	wantStatus(t, resp, http.StatusOK)
	if !strings.Contains(readAll(t, resp), "phone.js") {
		t.Error("phone page not served")
	}
	for header, want := range map[string]string{
		"Referrer-Policy":        "no-referrer",
		"X-Content-Type-Options": "nosniff",
		"Cache-Control":          "no-store",
	} {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}

	index := decode[struct {
		Name    string     `json:"name"`
		Receive bool       `json:"receive"`
		Items   []itemJSON `json:"items"`
	}](t, f.phone("GET", "/api/items", nil))
	if index.Name != "Test Mac" || !index.Receive || len(index.Items) != 0 {
		t.Errorf("index = %+v", index)
	}
}

func TestDownloadFile(t *testing.T) {
	f := newFixture(t)
	path := writeFile(t, filepath.Join(t.TempDir(), "Boarding pass.pdf"), "0123456789")
	f.share(path)

	resp := f.phone("GET", "/f/1/Boarding%20pass.pdf", nil)
	wantStatus(t, resp, http.StatusOK)
	if body := readAll(t, resp); body != "0123456789" {
		t.Errorf("body = %q", body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/pdf" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "filename*=UTF-8''Boarding%20pass.pdf") {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); csp != "sandbox" {
		t.Errorf("Content-Security-Policy = %q", csp)
	}

	transfers := f.srv.transfers.snapshot()
	if len(transfers) != 1 || transfers[0].State != transferDone || transfers[0].Bytes != 10 || transfers[0].Direction != toPhone {
		t.Fatalf("transfers = %+v", transfers)
	}

	// HEAD requests move no data and are not recorded.
	wantStatus(t, f.phone("HEAD", "/f/1/x", nil), http.StatusOK)
	if n := len(f.srv.transfers.snapshot()); n != 1 {
		t.Errorf("%d transfers after HEAD", n)
	}
	wantStatus(t, f.phone("GET", "/f/99/x", nil), http.StatusNotFound)
}

func TestResumedDownloadJoinsTheInterruptedTransfer(t *testing.T) {
	f := newFixture(t)
	f.share(writeFile(t, filepath.Join(t.TempDir(), "movie.mp4"), "0123456789"))

	// A first attempt that stopped after four bytes, as a download manager
	// would see it; then the resume.
	key := "127.0.0.1\x00" + "1" + "\x00" + "movie.mp4"
	first := f.srv.transfers.begin(key, "movie.mp4", toPhone, "127.0.0.1", 10, 0)
	first.add(4)
	if done := f.srv.transfers.end(first, false); done == nil || done.State != transferFailed {
		t.Fatalf("first attempt = %+v", done)
	}

	resp := f.phone("GET", "/f/1/movie.mp4", nil, "Range", "bytes=4-")
	wantStatus(t, resp, http.StatusPartialContent)
	if body := readAll(t, resp); body != "456789" {
		t.Errorf("body = %q", body)
	}
	transfers := f.srv.transfers.snapshot()
	if len(transfers) != 1 || transfers[0].State != transferDone || transfers[0].Bytes != 10 {
		t.Errorf("transfers = %+v", transfers)
	}
}

func TestReadingPartOfAFileIsNotATransfer(t *testing.T) {
	f := newFixture(t)
	f.share(writeFile(t, filepath.Join(t.TempDir(), "movie.mp4"), "0123456789"))
	resp := f.phone("GET", "/f/1/movie.mp4", nil, "Range", "bytes=2-5")
	wantStatus(t, resp, http.StatusPartialContent)
	if body := readAll(t, resp); body != "2345" {
		t.Errorf("body = %q", body)
	}
	if ts := f.srv.transfers.snapshot(); len(ts) != 0 {
		t.Errorf("transfers = %+v, want none", ts)
	}
}

func TestParallelRangeRequestsAreOneTransfer(t *testing.T) {
	var ts transfers
	whole := ts.begin("k", "big.iso", toPhone, "phone", 100, 0)
	tail := ts.begin("k", "big.iso", toPhone, "phone", 100, 50)
	if whole != tail {
		t.Fatal("the second part started a new transfer")
	}
	whole.add(52) // cut off by the client once the tail took over
	if ts.end(whole, false) != nil {
		t.Fatal("settled while a part was still running")
	}
	tail.add(50)
	done := ts.end(tail, true)
	if done == nil || done.State != transferDone || done.Bytes != 100 {
		t.Fatalf("settled as %+v", done)
	}
	if again := ts.begin("k", "big.iso", toPhone, "phone", 100, 0); again == whole {
		t.Error("a new download joined a finished one")
	}
}

func TestFolderListing(t *testing.T) {
	f := newFixture(t)
	f.share(makeFolder(t))

	type listing struct {
		Name    string      `json:"name"`
		Path    string      `json:"path"`
		Entries []entryJSON `json:"entries"`
	}
	root := decode[listing](t, f.phone("GET", "/api/items/1/list", nil))
	var names []string
	for _, e := range root.Entries {
		names = append(names, e.Name)
	}
	if root.Name != "Trip" || !slices.Equal(names, []string{"a.txt", "day 2", "empty"}) {
		t.Errorf("root listing = %s %v; want the hidden file and the escaping link left out", root.Name, names)
	}

	sub := decode[listing](t, f.phone("GET", "/api/items/1/list?path=day%202", nil))
	if sub.Name != "day 2" || sub.Path != "day 2" || len(sub.Entries) != 1 || sub.Entries[0].Size != 5 {
		t.Errorf("sub listing = %+v", sub)
	}

	index := decode[struct {
		Items []itemJSON `json:"items"`
	}](t, f.phone("GET", "/api/items", nil))
	if it := index.Items[0]; it.Files != 2 || it.Size != 10 {
		t.Errorf("folder counted as %d files, %d bytes; want 2, 10", it.Files, it.Size)
	}

	for _, bad := range []string{"..", "../..", "day%202/..", "/etc", ".DS_Store"} {
		wantStatus(t, f.phone("GET", "/api/items/1/list?path="+bad, nil), http.StatusNotFound)
	}
}

func TestFolderDownloadsStayInsideTheFolder(t *testing.T) {
	f := newFixture(t)
	f.share(makeFolder(t))

	resp := f.phone("GET", "/f/1/day%202/b.txt", nil)
	wantStatus(t, resp, http.StatusOK)
	if body := readAll(t, resp); body != "bravo" {
		t.Errorf("body = %q", body)
	}
	for _, bad := range []string{
		"/f/1/escape.txt",           // symlink to a file outside
		"/f/1/.DS_Store",            // hidden
		"/f/1/..%2F..%2Fsecret.txt", // encoded traversal
		"/f/1/day%202",              // a folder, not a file
		"/f/1/",                     // the shared folder itself
	} {
		if resp := f.phone("GET", bad, nil); resp.StatusCode == http.StatusOK {
			t.Errorf("GET %s: status 200", bad)
		}
	}
}

func readZip(t *testing.T, resp *http.Response) map[string]string {
	t.Helper()
	body := []byte(readAll(t, resp))
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	files := make(map[string]string)
	for _, zf := range zr.File {
		rc, err := zf.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		files[zf.Name] = string(b)
	}
	return files
}

func TestZipDownloads(t *testing.T) {
	f := newFixture(t)
	f.share(makeFolder(t))
	f.share(writeFile(t, filepath.Join(t.TempDir(), "a.txt"), "first"))
	f.share(writeFile(t, filepath.Join(t.TempDir(), "a.txt"), "second"))

	resp := f.phone("GET", "/z/1/", nil)
	wantStatus(t, resp, http.StatusOK)
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, `filename="Trip.zip"`) {
		t.Errorf("Content-Disposition = %q", cd)
	}
	got := readZip(t, resp)
	want := map[string]string{
		"Trip/":            "",
		"Trip/a.txt":       "alpha",
		"Trip/day 2/":      "",
		"Trip/day 2/b.txt": "bravo",
		"Trip/empty/":      "",
	}
	if !mapsEqual(got, want) {
		t.Errorf("folder zip = %v\nwant %v", got, want)
	}

	got = readZip(t, f.phone("GET", "/z/1/day%202", nil))
	if !mapsEqual(got, map[string]string{"day 2/": "", "day 2/b.txt": "bravo"}) {
		t.Errorf("subfolder zip = %v", got)
	}

	got = readZip(t, f.phone("GET", "/z/all", nil))
	if got["a.txt"] != "second" || got["a (2).txt"] != "first" || got["Trip/a.txt"] != "alpha" {
		t.Errorf("zip of everything = %v; want both a.txt files under unique names", got)
	}
	for _, s := range f.srv.transfers.snapshot() {
		if s.State != transferDone {
			t.Errorf("transfer %+v not done", s)
		}
	}

	wantStatus(t, f.phone("GET", "/z/2/", nil), http.StatusNotFound) // a file, not a folder
	wantStatus(t, f.phone("GET", "/z/1/..%2F..", nil), http.StatusNotFound)
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

func TestUpload(t *testing.T) {
	f := newFixture(t)
	mod := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	query := "/upload?name=..%2Fevil%2Fpho%3Ato.jpg&modified=" + strconv.FormatInt(mod.UnixMilli(), 10)

	resp := f.phone("POST", query, strings.NewReader("jpeg bytes"))
	wantStatus(t, resp, http.StatusOK)
	saved := decode[struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	}](t, resp)
	if saved.Name != "pho_to.jpg" || saved.Size != 10 {
		t.Errorf("saved = %+v", saved)
	}
	path := filepath.Join(f.receive, "pho_to.jpg")
	if b, _ := os.ReadFile(path); string(b) != "jpeg bytes" {
		t.Errorf("file = %q", b)
	}
	if info, err := os.Stat(path); err != nil || !info.ModTime().Equal(mod) {
		t.Errorf("modification time not kept: %v, %v", info.ModTime(), err)
	}

	// The same name again gets a number rather than replacing the file.
	resp = f.phone("POST", query, strings.NewReader("more"))
	wantStatus(t, resp, http.StatusOK)
	if b, _ := os.ReadFile(filepath.Join(f.receive, "pho_to (1).jpg")); string(b) != "more" {
		t.Errorf("second file = %q", b)
	}

	received := f.srv.received.list()
	if len(received) != 2 || received[0].Name != "pho_to (1).jpg" || received[1].Path != path {
		t.Errorf("received = %+v", received)
	}
	if entries, _ := os.ReadDir(f.receive); len(entries) != 2 {
		t.Errorf("receive folder holds %d entries, want 2 (no partial files)", len(entries))
	}
}

func TestUploadCutShortLeavesNothing(t *testing.T) {
	f := newFixture(t)
	// Claims 100 bytes but sends 5, as a phone that lost Wi-Fi would.
	req := httptest.NewRequest("POST", "/"+testToken+"/upload?name=big.bin", strings.NewReader("12345"))
	req.ContentLength = 100
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status %d", rec.Code)
	}
	if entries, _ := os.ReadDir(f.receive); len(entries) != 0 {
		t.Errorf("left behind: %v", entries)
	}
	if ts := f.srv.transfers.snapshot(); len(ts) != 1 || ts[0].State != transferFailed {
		t.Errorf("transfers = %+v", ts)
	}
}

func TestUploadRefusedWhenReceivingIsOff(t *testing.T) {
	s := New(Config{Token: testToken, AdminKey: testKey, Logf: t.Logf})
	defer s.Close()
	req := httptest.NewRequest("POST", "/"+testToken+"/upload?name=a.txt", strings.NewReader("x"))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status %d, want 403", rec.Code)
	}
}

func TestDesktopIsOnlyForThisComputer(t *testing.T) {
	f := newFixture(t)
	serve := func(remote, host, key string) int {
		req := httptest.NewRequest("GET", "/api/state", nil)
		req.RemoteAddr = remote
		req.Host = host
		if key != "" {
			req.Header.Set("X-Admin-Key", key)
		}
		rec := httptest.NewRecorder()
		f.srv.ServeHTTP(rec, req)
		return rec.Code
	}
	for _, tt := range []struct {
		remote, host, key string
		want              int
	}{
		{"127.0.0.1:5000", "localhost:8686", testKey, http.StatusOK},
		{"[::1]:5000", "[::1]:8686", testKey, http.StatusOK},
		{"127.0.0.1:5000", "127.0.0.1:8686", testKey, http.StatusOK},
		{"192.168.1.9:5000", "192.168.1.5:8686", testKey, http.StatusNotFound}, // from the LAN
		{"192.168.1.9:5000", "localhost:8686", testKey, http.StatusNotFound},   // from the LAN, forged Host
		{"127.0.0.1:5000", "evil.example:8686", testKey, http.StatusNotFound},  // DNS rebinding
		{"127.0.0.1:5000", "localhost:8686", "", http.StatusUnauthorized},
		{"127.0.0.1:5000", "localhost:8686", "wrong", http.StatusUnauthorized},
	} {
		if got := serve(tt.remote, tt.host, tt.key); got != tt.want {
			t.Errorf("from %s to %s with key %q: %d, want %d", tt.remote, tt.host, tt.key, got, tt.want)
		}
	}

	// The key is accepted in the query only for GET, for EventSource.
	wantStatus(t, f.do("GET", "/api/ping?key="+testKey, nil), http.StatusNoContent)
	wantStatus(t, f.do("POST", "/api/items?key="+testKey, strings.NewReader(`{"paths":[]}`)), http.StatusUnauthorized)
}

func TestDesktopState(t *testing.T) {
	f := newFixture(t)
	f.share(writeFile(t, filepath.Join(t.TempDir(), "notes.txt"), "hello"))
	st := decode[desktopStateJSON](t, f.admin("GET", "/api/state", nil))
	if len(st.Links) != 1 || !strings.HasPrefix(st.Links[0].QR, "<svg") {
		t.Errorf("links = %+v", st.Links)
	}
	if len(st.Items) != 1 || st.Items[0].Name != "notes.txt" || st.Items[0].Size != 5 || st.Items[0].Staged {
		t.Errorf("items = %+v", st.Items)
	}
	if !st.Receive.Enabled || st.Receive.Path != f.receive {
		t.Errorf("receive = %+v", st.Receive)
	}
}

func TestAddAndRemoveItems(t *testing.T) {
	f := newFixture(t)
	a := writeFile(t, filepath.Join(t.TempDir(), "a.txt"), "a")
	body := `{"paths":[` + quote(a) + `,` + quote(a) + `,"relative.txt",` + quote(filepath.Join(t.TempDir(), "missing")) + `]}`
	results := decode[[]struct {
		Added bool   `json:"added"`
		Error string `json:"error"`
	}](t, f.admin("POST", "/api/items", strings.NewReader(body)))
	if len(results) != 4 || !results[0].Added || results[1].Added || results[1].Error != "" ||
		results[2].Error == "" || results[3].Error == "" {
		t.Errorf("results = %+v", results)
	}
	if n := len(f.srv.items.list()); n != 1 {
		t.Fatalf("%d items shared, want 1", n)
	}
	wantStatus(t, f.admin("DELETE", "/api/items/1", nil), http.StatusNoContent)
	wantStatus(t, f.admin("DELETE", "/api/items/1", nil), http.StatusNotFound)
	if _, err := os.Stat(a); err != nil {
		t.Error("removing a shared path deleted the original:", err)
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestStagedFolder(t *testing.T) {
	f := newFixture(t)
	const stage = "0123456789abcdef"
	for path, content := range map[string]string{"Photos/a.jpg": "aaa", "Photos/sub/b.jpg": "bb"} {
		resp := f.do("PUT", "/api/stage/"+stage+"/"+path+"?modified=1700000000000", strings.NewReader(content), "X-Admin-Key", testKey)
		wantStatus(t, resp, http.StatusNoContent)
	}
	resp := f.admin("POST", "/api/stage/"+stage+"/share", strings.NewReader(`{"name":"Photos","dir":true}`))
	wantStatus(t, resp, http.StatusOK)

	got := readZip(t, f.phone("GET", "/z/1/", nil))
	if got["Photos/a.jpg"] != "aaa" || got["Photos/sub/b.jpg"] != "bb" {
		t.Errorf("zip = %v", got)
	}
	info, err := os.Stat(filepath.Join(f.staging, stage, "Photos", "a.jpg"))
	if err != nil || info.ModTime().UnixMilli() != 1700000000000 {
		t.Errorf("staged file time: %v, %v", info, err)
	}

	// A shared stage cannot be discarded; removing the item deletes it.
	wantStatus(t, f.admin("DELETE", "/api/stage/"+stage, nil), http.StatusConflict)
	wantStatus(t, f.admin("DELETE", "/api/items/1", nil), http.StatusNoContent)
	if _, err := os.Stat(filepath.Join(f.staging, stage)); !os.IsNotExist(err) {
		t.Errorf("staged copy still there: %v", err)
	}
}

func TestStagingRejectsBadPaths(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{
		"/api/stage/short/a.txt",                    // stage id too short
		"/api/stage/0123456789abcdef/..%2Fa.txt",    // traversal
		"/api/stage/0123456789abcdef/a%5C..%5Cb",    // backslashes
		"/api/stage/0123456789abcdef/x/..%2F..%2Fy", // traversal deeper down
	} {
		resp := f.do("PUT", path, strings.NewReader("x"), "X-Admin-Key", testKey)
		if resp.StatusCode == http.StatusNoContent {
			t.Errorf("PUT %s accepted", path)
		}
	}
	for _, name := range []string{`""`, `".."`, `"a/b"`, `"."`} {
		resp := f.admin("POST", "/api/stage/0123456789abcdef/share", strings.NewReader(`{"name":`+name+`}`))
		wantStatus(t, resp, http.StatusBadRequest)
	}
	entries, _ := os.ReadDir(f.staging)
	for _, e := range entries {
		// Only stage directories may exist, and nothing may have escaped.
		if !stageID.MatchString(e.Name()) {
			t.Errorf("unexpected entry in staging: %s", e.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(f.staging), "a.txt")); err == nil {
		t.Error("a file escaped the staging directory")
	}
}

func TestRevealOnlyReceivedFiles(t *testing.T) {
	f := newFixture(t)
	wantStatus(t, f.phone("POST", "/upload?name=a.txt", strings.NewReader("a")), http.StatusOK)
	received := filepath.Join(f.receive, "a.txt")
	for path, want := range map[string]int{
		received:        http.StatusNoContent,
		f.receive:       http.StatusNoContent,
		"/etc/passwd":   http.StatusNotFound,
		f.staging:       http.StatusNotFound,
		received + "/x": http.StatusNotFound,
	} {
		wantStatus(t, f.admin("POST", "/api/reveal", strings.NewReader(`{"path":`+quote(path)+`}`)), want)
	}
	if !slices.Equal(f.revealed, []string{received, f.receive}) && !slices.Equal(f.revealed, []string{f.receive, received}) {
		t.Errorf("revealed %v", f.revealed)
	}
}

// nextEvent reads server-sent events until one named name arrives.
func nextEvent(t *testing.T, r *bufio.Reader, name string) string {
	t.Helper()
	event := ""
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("waiting for %q: %v", name, err)
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: ") && event == name:
			return strings.TrimPrefix(line, "data: ")
		}
	}
}

func TestEventsFollowChanges(t *testing.T) {
	f := newFixture(t)
	phone := bufio.NewReader(f.phone("GET", "/api/events", nil).Body)
	desk := bufio.NewReader(f.do("GET", "/api/events?key="+testKey, nil).Body)

	// The desktop stream opens with the full state.
	var st desktopStateJSON
	if err := json.Unmarshal([]byte(nextEvent(t, desk, eventState)), &st); err != nil || st.Name != "Test Mac" {
		t.Fatalf("first desktop event: %+v, %v", st, err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		nextEvent(t, phone, eventChanged)
	}()
	f.share(writeFile(t, filepath.Join(t.TempDir(), "new.txt"), "x"))
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the phone was not told about the new item")
	}
	if err := json.Unmarshal([]byte(nextEvent(t, desk, eventState)), &st); err != nil || len(st.Items) != 1 {
		t.Fatalf("desktop state after sharing: %+v, %v", st.Items, err)
	}

	readAll(t, f.phone("GET", "/f/1/new.txt", nil))
	var transfers []transferJSON
	if err := json.Unmarshal([]byte(nextEvent(t, desk, eventTransfers)), &transfers); err != nil || len(transfers) != 1 {
		t.Fatalf("transfer progress: %+v, %v", transfers, err)
	}
}

func TestCloseEndsEventStreams(t *testing.T) {
	f := newFixture(t)
	resp := f.phone("GET", "/api/events", nil)
	f.srv.Close()
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, resp.Body)
		done <- err
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("event stream still open after Close")
	}
}

func TestZipFailureCutsTheResponseShort(t *testing.T) {
	f := newFixture(t)
	z := &zipBuilder{entries: []zipEntry{
		{name: "fine.txt", size: 4, open: func() (fs.File, error) { return os.Open(writeFile(t, filepath.Join(t.TempDir(), "fine.txt"), "fine")) }},
		{name: "gone.txt", size: 4, open: func() (fs.File, error) { return nil, fs.ErrNotExist }},
	}}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.srv.sendZip(w, r, "broken.zip", z)
	}))
	defer ts.Close()
	// Without the abort the body would end cleanly and the phone would
	// keep an archive that is missing its central directory. Depending on
	// how much was flushed, the error shows up in the headers or the body.
	resp, err := http.Get(ts.URL)
	if err == nil {
		_, err = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	if err == nil {
		t.Error("the broken archive downloaded without an error")
	}
	if ts := f.srv.transfers.snapshot(); len(ts) != 1 || ts[0].State != transferFailed {
		t.Errorf("transfers = %+v", ts)
	}
}

func TestQuit(t *testing.T) {
	quit := make(chan struct{})
	s := New(Config{Token: testToken, AdminKey: testKey, Logf: t.Logf, Quit: func() { close(quit) }})
	defer s.Close()
	req := httptest.NewRequest("POST", "http://localhost/api/quit", nil)
	req.RemoteAddr = "127.0.0.1:5000"
	req.Header.Set("X-Admin-Key", testKey)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d", rec.Code)
	}
	select {
	case <-quit:
	default:
		t.Error("Quit was not called")
	}
}
