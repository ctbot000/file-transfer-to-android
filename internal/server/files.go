package server

import (
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Go's built-in MIME table is small and the rest comes from the host
// system, so the same file would be labelled differently on each platform.
// These cover what people usually send to a phone. The APK type matters
// most: with it, Android offers to install the download.
func init() {
	for ext, typ := range map[string]string{
		".apk":  "application/vnd.android.package-archive",
		".aab":  "application/octet-stream",
		".epub": "application/epub+zip",
		".zip":  "application/zip",
		".pdf":  "application/pdf",
		".txt":  "text/plain; charset=utf-8",
		".csv":  "text/csv; charset=utf-8",
		".srt":  "application/x-subrip",
		".jpg":  "image/jpeg",
		".jpeg": "image/jpeg",
		".png":  "image/png",
		".gif":  "image/gif",
		".webp": "image/webp",
		".heic": "image/heic",
		".heif": "image/heif",
		".avif": "image/avif",
		".mp4":  "video/mp4",
		".m4v":  "video/mp4",
		".mov":  "video/quicktime",
		".mkv":  "video/x-matroska",
		".webm": "video/webm",
		".avi":  "video/x-msvideo",
		".3gp":  "video/3gpp",
		".mp3":  "audio/mpeg",
		".m4a":  "audio/mp4",
		".aac":  "audio/aac",
		".flac": "audio/flac",
		".wav":  "audio/wav",
		".ogg":  "audio/ogg",
		".opus": "audio/ogg",
		".doc":  "application/msword",
		".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		".xls":  "application/vnd.ms-excel",
		".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		".ppt":  "application/vnd.ms-powerpoint",
		".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	} {
		_ = mime.AddExtensionType(ext, typ)
	}
}

func contentType(name string) string {
	if t := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); t != "" {
		return t
	}
	return "application/octet-stream"
}

// contentDisposition builds a Content-Disposition value that survives any
// file name: an ASCII approximation in filename for old clients, and the
// exact name as an RFC 5987 ext-value in filename*, which browsers prefer.
func contentDisposition(kind, name string) string {
	var fallback strings.Builder
	for _, r := range name {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			fallback.WriteByte('_')
		} else {
			fallback.WriteRune(r)
		}
	}
	return fmt.Sprintf(`%s; filename="%s"; filename*=UTF-8''%s`, kind, fallback.String(), rfc5987Escape(name))
}

func rfc5987Escape(s string) string {
	const attrChars = "!#$&+-.^_`|~"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte(attrChars, c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// maxNameBytes leaves room under the usual 255-byte limit for a " (99)"
// suffix and the ".part" of a file still being received.
const maxNameBytes = 200

// sanitizeFilename turns a name chosen by the sending device into one that
// is safe to create on any desktop OS: a single path element, no characters
// Windows or macOS reject, not hidden, not a Windows device name, and not
// too long.
func sanitizeFilename(name string) string {
	name = path.Base(strings.ReplaceAll(name, `\`, "/"))
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20 || r == 0x7f || r == utf8.RuneError:
			// Control characters and invalid UTF-8 are dropped.
		case strings.ContainsRune(`<>:"/|?*`, r):
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	// A leading dot would hide the file; Windows rejects trailing dots and spaces.
	name = strings.Trim(b.String(), " .")
	if name == "" {
		name = "file"
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	if isWindowsDeviceName(stem) {
		stem = "_" + stem
	}
	if len(ext) > 20 {
		stem, ext = stem+ext, ""
	}
	if room := maxNameBytes - len(ext); len(stem) > room {
		stem = truncateUTF8(stem, room)
	}
	return stem + ext
}

func isWindowsDeviceName(stem string) bool {
	switch s := strings.ToUpper(strings.TrimRight(stem, " ")); s {
	case "CON", "PRN", "AUX", "NUL":
		return true
	default:
		return len(s) == 4 && (strings.HasPrefix(s, "COM") || strings.HasPrefix(s, "LPT")) && s[3] >= '1' && s[3] <= '9'
	}
}

func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// numbered returns name with " (i)" inserted before its extension, or name
// itself when i is 0: "photo.jpg", "photo (1).jpg", "photo (2).jpg", ...
func numbered(name string, i int) string {
	if i == 0 {
		return name
	}
	ext := filepath.Ext(name)
	return fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(name, ext), i, ext)
}

const partSuffix = ".part"

// incoming is a file being received. It is written to part and renamed to
// final once complete, so a half-received file never has the real name.
type incoming struct {
	*os.File
	dir   string
	name  string // the sanitized name the sender asked for
	index int    // the number suffix final has; see numbered
	final string
	part  string
}

// reserveFile creates the partial file for an incoming file named name,
// choosing the first of "name", "name (1)", ... for which neither the file
// nor its partial file exists. The exclusive create stops concurrent
// uploads of the same name from picking the same one.
func reserveFile(dir, name string) (*incoming, error) {
	for i := 0; i < 10000; i++ {
		final := filepath.Join(dir, numbered(name, i))
		if exists, err := pathExists(final); err != nil {
			return nil, err
		} else if exists {
			continue
		}
		f, err := os.OpenFile(final+partSuffix, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return &incoming{File: f, dir: dir, name: name, index: i, final: final, part: final + partSuffix}, nil
	}
	return nil, fmt.Errorf("too many files named %q", name)
}

// commit gives the finished (and closed) file its final name and returns
// its path. If something else took that name in the meantime, the next
// free number is used rather than overwriting it.
func (in *incoming) commit() (string, error) {
	for i := in.index; i < in.index+10000; i++ {
		candidate := filepath.Join(in.dir, numbered(in.name, i))
		exists, err := pathExists(candidate)
		if err != nil {
			return "", err
		}
		if exists {
			continue
		}
		if i != in.index {
			// Another upload may be writing this name's partial file.
			if busy, err := pathExists(candidate + partSuffix); err != nil || busy {
				continue
			}
		}
		return candidate, os.Rename(in.part, candidate)
	}
	return "", fmt.Errorf("too many files named %q", in.name)
}

func pathExists(p string) (bool, error) {
	_, err := os.Lstat(p)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

// hidden reports whether a directory entry is one people don't mean to
// send: dotfiles such as .DS_Store, and Windows' thumbnail and folder
// settings files.
func hidden(name string) bool {
	return strings.HasPrefix(name, ".") || strings.EqualFold(name, "Thumbs.db") || strings.EqualFold(name, "desktop.ini")
}

// hiddenPath reports whether any element of a slash-separated path is hidden.
func hiddenPath(p string) bool {
	for _, elem := range strings.Split(p, "/") {
		if hidden(elem) {
			return true
		}
	}
	return false
}

// walkFiles calls fn for every regular file under dir in fsys, skipping
// hidden entries. A symbolic link is followed when it points to a regular
// file inside fsys; links to folders are skipped, which also rules out
// cycles. Entries that cannot be read are skipped rather than failing the
// whole walk.
func walkFiles(fsys fs.FS, dir string, fn func(p string, d fs.DirEntry, info fs.FileInfo) error) error {
	return fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == dir {
				return err
			}
			return nil
		}
		if p != dir && hidden(d.Name()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return fn(p, d, nil)
		}
		info, err := fileInfo(fsys, p, d)
		if err != nil || info == nil {
			return nil
		}
		return fn(p, d, info)
	})
}

// fileInfo returns the FileInfo of a regular file entry, following a
// symbolic link to one, or nil for anything else.
func fileInfo(fsys fs.FS, p string, d fs.DirEntry) (fs.FileInfo, error) {
	switch {
	case d.Type().IsRegular():
		return d.Info()
	case d.Type()&fs.ModeSymlink != 0:
		info, err := fs.Stat(fsys, p)
		if err != nil || !info.Mode().IsRegular() {
			return nil, err
		}
		return info, nil
	default:
		return nil, nil
	}
}

// DisplayPath shows a path under the home folder as "~/...", which is
// shorter and does not put the account name on screen.
func DisplayPath(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if p == home {
		return "~"
	}
	if rel, err := filepath.Rel(home, p); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "~" + string(filepath.Separator) + rel
	}
	return p
}
