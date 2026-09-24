package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizeFilename(t *testing.T) {
	tests := []struct{ in, want string }{
		{"photo.jpg", "photo.jpg"},
		{"../../etc/passwd", "passwd"},
		{`C:\Users\me\report.pdf`, "report.pdf"},
		{"a:b*c?d<e>f|g\"h.txt", "a_b_c_d_e_f_g_h.txt"},
		{".bashrc", "bashrc"},
		{"trailing. ", "trailing"},
		{"", "file"},
		{"..", "file"},
		{"/", "_"},
		{"tab\there\x00.txt", "tabhere.txt"},
		{"CON.txt", "_CON.txt"},
		{"com1", "_com1"},
		{"COM10.txt", "COM10.txt"},
		{"사진 2026.heic", "사진 2026.heic"},
	}
	for _, tt := range tests {
		if got := sanitizeFilename(tt.in); got != tt.want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSanitizeFilenameLimitsLength(t *testing.T) {
	long := strings.Repeat("가", 150) + ".jpg" // 450 bytes of Hangul
	got := sanitizeFilename(long)
	if len(got) > maxNameBytes {
		t.Fatalf("len = %d, want <= %d", len(got), maxNameBytes)
	}
	if !strings.HasSuffix(got, ".jpg") {
		t.Fatalf("extension lost: %q", got)
	}
	if !strings.HasPrefix(got, "가") || strings.ContainsRune(got, '\uFFFD') {
		t.Fatalf("truncated inside a character: %q", got)
	}
}

func TestContentDisposition(t *testing.T) {
	got := contentDisposition("attachment", `Ünïcode "name".pdf`)
	want := `attachment; filename="_n_code _name_.pdf"; filename*=UTF-8''%C3%9Cn%C3%AFcode%20%22name%22.pdf`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestNumbered(t *testing.T) {
	for _, tt := range []struct {
		name string
		i    int
		want string
	}{
		{"photo.jpg", 0, "photo.jpg"},
		{"photo.jpg", 1, "photo (1).jpg"},
		{"archive.tar.gz", 2, "archive.tar (2).gz"},
		{"README", 3, "README (3)"},
	} {
		if got := numbered(tt.name, tt.i); got != tt.want {
			t.Errorf("numbered(%q, %d) = %q, want %q", tt.name, tt.i, got, tt.want)
		}
	}
}

func TestReserveAndCommitNeverOverwrite(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	in1, err := reserveFile(dir, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	in2, err := reserveFile(dir, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	final1, final2 := in1.final, in2.final
	if filepath.Base(final1) != "a (1).txt" || filepath.Base(final2) != "a (2).txt" {
		t.Fatalf("reserved %q and %q", final1, final2)
	}
	in1.WriteString("one")
	in1.Close()
	in2.WriteString("two")
	in2.Close()

	// Someone else takes "a (1).txt" before the first upload finishes.
	if err := os.WriteFile(final1, []byte("intruder"), 0o644); err != nil {
		t.Fatal(err)
	}
	got1, err := in1.commit()
	if err != nil {
		t.Fatal(err)
	}
	got2, err := in2.commit()
	if err != nil {
		t.Fatal(err)
	}
	if got2 != final2 {
		t.Errorf("second upload moved to %q, want %q", got2, final2)
	}
	if filepath.Base(got1) != "a (3).txt" {
		t.Errorf("first upload moved to %q, want a (3).txt", got1)
	}
	for path, want := range map[string]string{
		filepath.Join(dir, "a.txt"): "old",
		final1:                      "intruder",
		got1:                        "one",
		got2:                        "two",
	} {
		if b, _ := os.ReadFile(path); string(b) != want {
			t.Errorf("%s = %q, want %q", filepath.Base(path), b, want)
		}
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "*"+partSuffix)); len(matches) > 0 {
		t.Errorf("partial files left behind: %v", matches)
	}
}

func TestFormatSize(t *testing.T) {
	for n, want := range map[int64]string{
		0:             "0 B",
		999:           "999 B",
		1000:          "1.00 kB",
		1234:          "1.23 kB",
		56_700:        "56.7 kB",
		999_999:       "1.00 MB",
		123_456_789:   "123 MB",
		4_700_000_000: "4.70 GB",
	} {
		if got := formatSize(n); got != want {
			t.Errorf("formatSize(%d) = %q, want %q", n, got, want)
		}
	}
}
