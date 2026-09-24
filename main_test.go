package main

import (
	"io"
	"slices"
	"strings"
	"testing"
)

func TestParseOptionsAcceptsFlagsAnywhere(t *testing.T) {
	opts, err := parseOptions([]string{"a.jpg", "-no-browser", "b dir", "--port", "9000", "--", "-weird-name.txt"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !opts.noBrowser || opts.port != 9000 || !opts.portSet {
		t.Errorf("flags not applied: %+v", opts)
	}
	if want := []string{"a.jpg", "b dir", "-weird-name.txt"}; !slices.Equal(opts.paths, want) {
		t.Errorf("paths = %q, want %q", opts.paths, want)
	}

	opts, err = parseOptions(nil, io.Discard)
	if err != nil || opts.port != defaultPort || opts.portSet || len(opts.paths) != 0 {
		t.Errorf("defaults: %+v, %v", opts, err)
	}
	if _, err := parseOptions([]string{"-port", "70000"}, io.Discard); err == nil {
		t.Error("accepted port 70000")
	}
	if _, err := parseOptions([]string{"-nope"}, io.Discard); err == nil {
		t.Error("accepted an unknown flag")
	}
}

func TestRandomToken(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		tok := randomToken(12)
		if len(tok) != 12 || strings.Trim(tok, "abcdefghijklmnopqrstuvwxyz234567") != "" {
			t.Fatalf("bad token %q", tok)
		}
		if seen[tok] {
			t.Fatalf("repeated token %q", tok)
		}
		seen[tok] = true
	}
}

func TestPhoneLinkURL(t *testing.T) {
	l := &phoneLinks{port: 8686, token: "abc"}
	for host, want := range map[string]string{
		"192.168.1.23": "http://192.168.1.23:8686/abc/",
		"fd00::5":      "http://[fd00::5]:8686/abc/",
		"mymac.local":  "http://mymac.local:8686/abc/",
	} {
		if got := l.url(host); got != want {
			t.Errorf("url(%q) = %q, want %q", host, got, want)
		}
	}
}
