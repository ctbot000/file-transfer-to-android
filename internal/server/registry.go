package server

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"
)

// item is one file or folder being shared with the phone.
type item struct {
	ID    string
	Name  string
	Path  string // absolute
	Dir   bool
	Added time.Time
	// Staged is the staging directory holding this item when it was copied
	// in through the desktop page. It is deleted along with the item.
	Staged string

	statsMu sync.Mutex
	stats   folderStats
	statsAt time.Time
}

type folderStats struct {
	Files int
	Size  int64
}

// statsTTL bounds how often a shared folder is walked to count its files,
// however many clients are asking.
const statsTTL = 5 * time.Second

// folderStats counts the files a folder item would send and their total
// size, caching the answer briefly.
func (it *item) folderStats() folderStats {
	it.statsMu.Lock()
	defer it.statsMu.Unlock()
	if time.Since(it.statsAt) < statsTTL {
		return it.stats
	}
	var stats folderStats
	if root, err := os.OpenRoot(it.Path); err == nil {
		_ = walkFiles(root.FS(), ".", func(_ string, _ fs.DirEntry, info fs.FileInfo) error {
			if info != nil {
				stats.Files++
				stats.Size += info.Size()
			}
			return nil
		})
		root.Close()
	}
	it.stats, it.statsAt = stats, time.Now()
	return stats
}

// registry is the list of shared items, in the order they were added.
type registry struct {
	mu    sync.RWMutex
	items []*item
	last  int
}

var errNotShareable = errors.New("only files and folders can be shared")

// add shares the file or folder at path. Sharing a path that is already
// shared returns the existing item, with added set to false.
func (r *registry) add(path, staged string) (it *item, added bool, err error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, false, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, false, err
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("%s: %w", abs, errNotShareable)
	}
	name := filepath.Base(abs)
	if name == string(filepath.Separator) || name == "." || filepath.VolumeName(abs)+string(filepath.Separator) == abs {
		name = "Files"
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.items {
		if existing.Path == abs {
			return existing, false, nil
		}
	}
	r.last++
	it = &item{
		ID:     strconv.Itoa(r.last),
		Name:   name,
		Path:   abs,
		Dir:    info.IsDir(),
		Added:  time.Now(),
		Staged: staged,
	}
	r.items = append(r.items, it)
	return it, true, nil
}

func (r *registry) remove(id string) (*item, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, it := range r.items {
		if it.ID == id {
			r.items = slices.Delete(r.items, i, i+1)
			return it, true
		}
	}
	return nil, false
}

func (r *registry) get(id string) (*item, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, it := range r.items {
		if it.ID == id {
			return it, true
		}
	}
	return nil, false
}

// list returns the items, newest first.
func (r *registry) list() []*item {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := slices.Clone(r.items)
	slices.Reverse(out)
	return out
}

func (r *registry) usesStage(dir string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, it := range r.items {
		if it.Staged == dir {
			return true
		}
	}
	return false
}
