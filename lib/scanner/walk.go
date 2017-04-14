// Copyright (C) 2014 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package scanner

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/rcrowley/go-metrics"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/ignore"
	"github.com/syncthing/syncthing/lib/osutil"
	"github.com/syncthing/syncthing/lib/protocol"
	"golang.org/x/text/unicode/norm"
)

var (
	maskModePerm os.FileMode
	warnOnce     sync.Once
)

func init() {
	if runtime.GOOS == "windows" {
		// There is no user/group/others in Windows' read-only
		// attribute, and all "w" bits are set in os.FileInfo
		// if the file is not read-only.  Do not send these
		// group/others-writable bits to other devices in order to
		// avoid unexpected world-writable files on other platforms.
		maskModePerm = os.ModePerm & 0755
	} else {
		maskModePerm = os.ModePerm
	}
}

type Config struct {
	// Folder for which the walker has been created
	Folder string
	// Dir is the base directory for the walk
	Dir string
	// Limit walking to these paths within Dir, or no limit if Sub is empty
	Subs []string
	// BlockSize controls the size of the block used when hashing.
	BlockSize int
	// If Matcher is not nil, it is used to identify files to ignore which were specified by the user.
	Matcher *ignore.Matcher
	// Number of hours to keep temporary files for
	TempLifetime time.Duration
	// If CurrentFiler is not nil, it is queried for the current file before rescanning.
	CurrentFiler CurrentFiler
	// The Lstater provides reliable mtimes on top of the regular filesystem.
	Lstater Lstater
	// If IgnorePerms is true, changes to permission bits will not be
	// detected. Scanned files will get zero permission bits and the
	// NoPermissionBits flag set.
	IgnorePerms bool
	// When AutoNormalize is set, file names that are in UTF8 but incorrect
	// normalization form will be corrected.
	AutoNormalize bool
	// Number of routines to use for hashing
	Hashers int
	// Our vector clock id
	ShortID protocol.ShortID
	// Optional progress tick interval which defines how often FolderScanProgress
	// events are emitted. Negative number means disabled.
	ProgressTickIntervalS int
	// Signals cancel from the outside - when closed, we should stop walking.
	Cancel chan struct{}
	// Whether or not we should also compute weak hashes
	UseWeakHashes bool
	FollowSymlinks []string
}

type CurrentFiler interface {
	// CurrentFile returns the file as seen at last scan.
	CurrentFile(name string) (protocol.FileInfo, bool)
}

type Lstater interface {
	Lstat(name string) (os.FileInfo, error)
}

func Walk(cfg Config) (chan protocol.FileInfo, error) {
	w := walker{cfg}

	if w.CurrentFiler == nil {
		w.CurrentFiler = noCurrentFiler{}
	}
	if w.Lstater == nil {
		w.Lstater = defaultLstater{}
	}

	return w.walk()
}

type walker struct {
	Config
}

// Walk returns the list of files found in the local folder by scanning the
// file system. Files are blockwise hashed.
func (w *walker) walk() (chan protocol.FileInfo, error) {
	l.Debugln("Walk", w.Dir, w.Subs, w.BlockSize, w.Matcher)

	if err := w.checkDir(); err != nil {
		return nil, err
	}

	toHashChan := make(chan protocol.FileInfo)
	finishedChan := make(chan protocol.FileInfo)

	// A routine which walks the filesystem tree, and sends files which have
	// been modified to the counter routine.
	go func() {
		hashFiles := w.walkAndHashFiles(toHashChan, finishedChan)

		var dirs []string
		if len(w.Subs) == 0 {
			// The list of dirs to walk is the folder dir itself.
			dirs = []string{w.Dir}
		} else {
			// If a set of subdirs was given, replace the list with the list of
			// subdirs, relative to the folder path.
			for _, sub := range w.Subs {
				path := filepath.Join(w.Dir, sub)
				dirs = append(dirs, path)
			}
		}

		// If a there's a set of symlinks to follow, do the same for those.
		if len(w.FollowSymlinks) > 0 {
		nextSymlink:
			for _, link := range w.FollowSymlinks {
				path := filepath.Join(w.Dir, link)

				// Verify that the symlink is under one of the dirs we
				// intend to scan.
				for _, allowed := range dirs {
					if strings.HasPrefix(path, allowed+string(os.PathSeparator)) {
						goto ok
					}
				}
				continue nextSymlink

			ok:
				info, err := os.Stat(path)
				if err != nil {
					// The symlink points to something that doesn't exist. Never mind.
					continue
				}
				if !info.IsDir() {
					warnOnce.Do(func() {
						l.Warnf("Following symlinks to files is unsupported (%s).", path)
					})
					continue
				}

				// Append the path separator so that the scanner will
				// descend into the directory instead of seeing the symlink
				// itself.
				path += string(os.PathSeparator)
				dirs = append(dirs, path)
			}
		}

		for _, dir := range dirs {
			filepath.Walk(dir, hashFiles)
		}
		close(toHashChan)
	}()

	// We're not required to emit scan progress events, just kick off hashers,
	// and feed inputs directly from the walker.
	if w.ProgressTickIntervalS < 0 {
		newParallelHasher(w.Dir, w.BlockSize, w.Hashers, finishedChan, toHashChan, nil, nil, w.Cancel, w.UseWeakHashes)
		return finishedChan, nil
	}

	// Defaults to every 2 seconds.
	if w.ProgressTickIntervalS == 0 {
		w.ProgressTickIntervalS = 2
	}

	ticker := time.NewTicker(time.Duration(w.ProgressTickIntervalS) * time.Second)

	// We need to emit progress events, hence we create a routine which buffers
	// the list of files to be hashed, counts the total number of
	// bytes to hash, and once no more files need to be hashed (chan gets closed),
	// start a routine which periodically emits FolderScanProgress events,
	// until a stop signal is sent by the parallel hasher.
	// Parallel hasher is stopped by this routine when we close the channel over
	// which it receives the files we ask it to hash.
	go func() {
		var filesToHash []protocol.FileInfo
		var total int64 = 1

		for file := range toHashChan {
			filesToHash = append(filesToHash, file)
			total += file.Size
		}

		realToHashChan := make(chan protocol.FileInfo)
		done := make(chan struct{})
		progress := newByteCounter()

		newParallelHasher(w.Dir, w.BlockSize, w.Hashers, finishedChan, realToHashChan, progress, done, w.Cancel, w.UseWeakHashes)

		// A routine which actually emits the FolderScanProgress events
		// every w.ProgressTicker ticks, until the hasher routines terminate.
		go func() {
			defer progress.Close()

			for {
				select {
				case <-done:
					l.Debugln("Walk progress done", w.Dir, w.Subs, w.BlockSize, w.Matcher)
					ticker.Stop()
					return
				case <-ticker.C:
					current := progress.Total()
					rate := progress.Rate()
					l.Debugf("Walk %s %s current progress %d/%d at %.01f MiB/s (%d%%)", w.Dir, w.Subs, current, total, rate/1024/1024, current*100/total)
					events.Default.Log(events.FolderScanProgress, map[string]interface{}{
						"folder":  w.Folder,
						"current": current,
						"total":   total,
						"rate":    rate, // bytes per second
					})
				case <-w.Cancel:
					ticker.Stop()
					return
				}
			}
		}()

	loop:
		for _, file := range filesToHash {
			l.Debugln("real to hash:", file.Name)
			select {
			case realToHashChan <- file:
			case <-w.Cancel:
				break loop
			}
		}
		close(realToHashChan)
	}()

	return finishedChan, nil
}

func (w *walker) walkAndHashFiles(fchan, dchan chan protocol.FileInfo) filepath.WalkFunc {
	now := time.Now()
	return func(absPath string, info os.FileInfo, err error) error {
		// Return value used when we are returning early and don't want to
		// process the item. For directories, this means do-not-descend.
		var skip error // nil
		// info nil when error is not nil
		if info != nil && info.IsDir() {
			skip = filepath.SkipDir
		}

		if err != nil {
			l.Debugln("error:", absPath, info, err)
			return skip
		}

		relPath, err := filepath.Rel(w.Dir, absPath)
		if err != nil {
			l.Debugln("rel error:", absPath, err)
			return skip
		}

		if relPath == "." {
			return nil
		}

		info, err = w.Lstater.Lstat(absPath)
		// An error here would be weird as we've already gotten to this point, but act on it nonetheless
		if err != nil {
			return skip
		}

		if ignore.IsTemporary(relPath) {
			l.Debugln("temporary:", relPath)
			if info.Mode().IsRegular() && info.ModTime().Add(w.TempLifetime).Before(now) {
				os.Remove(absPath)
				l.Debugln("removing temporary:", relPath, info.ModTime())
			}
			return nil
		}

		if ignore.IsInternal(relPath) {
			l.Debugln("ignored (internal):", relPath)
			return skip
		}

		if w.Matcher.Match(relPath).IsIgnored() {
			l.Debugln("ignored (patterns):", relPath)
			return skip
		}

		if !utf8.ValidString(relPath) {
			l.Warnf("File name %q is not in UTF8 encoding; skipping.", relPath)
			return skip
		}

		relPath, shouldSkip := w.normalizePath(absPath, relPath)
		if shouldSkip {
			return skip
		}

		switch {
		case info.Mode()&os.ModeSymlink == os.ModeSymlink:
			for _, link := range w.FollowSymlinks {
				// If the symlink is one of those we are supposed to follow,
				// we should not treat it as a symlink when seeing it here.
				if relPath == link {
					return skip
				}
			}

			if err := w.walkSymlink(absPath, relPath, dchan); err != nil {
				return err
			}
			if info.IsDir() {
				// under no circumstances shall we descend into a symlink
				return filepath.SkipDir
			}
			return nil

		case info.Mode().IsDir():
			err = w.walkDir(relPath, info, dchan)

		case info.Mode().IsRegular():
			err = w.walkRegular(relPath, info, fchan)
		}

		return err
	}
}

func (w *walker) walkRegular(relPath string, info os.FileInfo, fchan chan protocol.FileInfo) error {
	curMode := uint32(info.Mode())
	if runtime.GOOS == "windows" && osutil.IsWindowsExecutable(relPath) {
		curMode |= 0111
	}

	// A file is "unchanged", if it
	//  - exists
	//  - has the same permissions as previously, unless we are ignoring permissions
	//  - was not marked deleted (since it apparently exists now)
	//  - had the same modification time as it has now
	//  - was not a directory previously (since it's a file now)
	//  - was not a symlink (since it's a file now)
	//  - was not invalid (since it looks valid now)
	//  - has the same size as previously
	cf, ok := w.CurrentFiler.CurrentFile(relPath)
	permUnchanged := w.IgnorePerms || !cf.HasPermissionBits() || PermsEqual(cf.Permissions, curMode)
	if ok && permUnchanged && !cf.IsDeleted() && cf.ModTime().Equal(info.ModTime()) && !cf.IsDirectory() &&
		!cf.IsSymlink() && !cf.IsInvalid() && cf.Size == info.Size() {
		return nil
	}

	if ok {
		l.Debugln("rescan:", cf, info.ModTime().Unix(), info.Mode()&os.ModePerm)
	}

	f := protocol.FileInfo{
		Name:          relPath,
		Type:          protocol.FileInfoTypeFile,
		Version:       cf.Version.Update(w.ShortID),
		Permissions:   curMode & uint32(maskModePerm),
		NoPermissions: w.IgnorePerms,
		ModifiedS:     info.ModTime().Unix(),
		ModifiedNs:    int32(info.ModTime().Nanosecond()),
		ModifiedBy:    w.ShortID,
		Size:          info.Size(),
	}
	l.Debugln("to hash:", relPath, f)

	select {
	case fchan <- f:
	case <-w.Cancel:
		return errors.New("cancelled")
	}

	return nil
}

func (w *walker) walkDir(relPath string, info os.FileInfo, dchan chan protocol.FileInfo) error {
	// A directory is "unchanged", if it
	//  - exists
	//  - has the same permissions as previously, unless we are ignoring permissions
	//  - was not marked deleted (since it apparently exists now)
	//  - was a directory previously (not a file or something else)
	//  - was not a symlink (since it's a directory now)
	//  - was not invalid (since it looks valid now)
	cf, ok := w.CurrentFiler.CurrentFile(relPath)
	permUnchanged := w.IgnorePerms || !cf.HasPermissionBits() || PermsEqual(cf.Permissions, uint32(info.Mode()))
	if ok && permUnchanged && !cf.IsDeleted() && cf.IsDirectory() && !cf.IsSymlink() && !cf.IsInvalid() {
		return nil
	}

	f := protocol.FileInfo{
		Name:          relPath,
		Type:          protocol.FileInfoTypeDirectory,
		Version:       cf.Version.Update(w.ShortID),
		Permissions:   uint32(info.Mode() & maskModePerm),
		NoPermissions: w.IgnorePerms,
		ModifiedS:     info.ModTime().Unix(),
		ModifiedNs:    int32(info.ModTime().Nanosecond()),
		ModifiedBy:    w.ShortID,
	}
	l.Debugln("dir:", relPath, f)

	select {
	case dchan <- f:
	case <-w.Cancel:
		return errors.New("cancelled")
	}

	return nil
}

// walkSymlink returns nil or an error, if the error is of the nature that
// it should stop the entire walk.
func (w *walker) walkSymlink(absPath, relPath string, dchan chan protocol.FileInfo) error {
	// Symlinks are not supported on Windows. We ignore instead of returning
	// an error.
	if runtime.GOOS == "windows" {
		return nil
	}

	// We always rehash symlinks as they have no modtime or
	// permissions. We check if they point to the old target by
	// checking that their existing blocks match with the blocks in
	// the index.

	target, err := os.Readlink(absPath)
	if err != nil {
		l.Debugln("readlink error:", absPath, err)
		return nil
	}

	// A symlink is "unchanged", if
	//  - it exists
	//  - it wasn't deleted (because it isn't now)
	//  - it was a symlink
	//  - it wasn't invalid
	//  - the symlink type (file/dir) was the same
	//  - the target was the same
	cf, ok := w.CurrentFiler.CurrentFile(relPath)
	if ok && !cf.IsDeleted() && cf.IsSymlink() && !cf.IsInvalid() && cf.SymlinkTarget == target {
		return nil
	}

	f := protocol.FileInfo{
		Name:          relPath,
		Type:          protocol.FileInfoTypeSymlink,
		Version:       cf.Version.Update(w.ShortID),
		NoPermissions: true, // Symlinks don't have permissions of their own
		SymlinkTarget: target,
	}

	l.Debugln("symlink changedb:", absPath, f)

	select {
	case dchan <- f:
	case <-w.Cancel:
		return errors.New("cancelled")
	}

	return nil
}

// normalizePath returns the normalized relative path (possibly after fixing
// it on disk), or skip is true.
func (w *walker) normalizePath(absPath, relPath string) (normPath string, skip bool) {
	if runtime.GOOS == "darwin" {
		// Mac OS X file names should always be NFD normalized.
		normPath = norm.NFD.String(relPath)
	} else {
		// Every other OS in the known universe uses NFC or just plain
		// doesn't bother to define an encoding. In our case *we* do care,
		// so we enforce NFC regardless.
		normPath = norm.NFC.String(relPath)
	}

	if relPath != normPath {
		// The file name was not normalized.

		if !w.AutoNormalize {
			// We're not authorized to do anything about it, so complain and skip.

			l.Warnf("File name %q is not in the correct UTF8 normalization form; skipping.", relPath)
			return "", true
		}

		// We will attempt to normalize it.
		normalizedPath := filepath.Join(w.Dir, normPath)
		if _, err := w.Lstater.Lstat(normalizedPath); os.IsNotExist(err) {
			// Nothing exists with the normalized filename. Good.
			if err = os.Rename(absPath, normalizedPath); err != nil {
				l.Infof(`Error normalizing UTF8 encoding of file "%s": %v`, relPath, err)
				return "", true
			}
			l.Infof(`Normalized UTF8 encoding of file name "%s".`, relPath)
		} else {
			// There is something already in the way at the normalized
			// file name.
			l.Infof(`File "%s" has UTF8 encoding conflict with another file; ignoring.`, relPath)
			return "", true
		}
	}

	return normPath, false
}

func (w *walker) checkDir() error {
	if info, err := w.Lstater.Lstat(w.Dir); err != nil {
		return err
	} else if !info.IsDir() {
		return errors.New(w.Dir + ": not a directory")
	} else {
		l.Debugln("checkDir", w.Dir, info)
	}
	return nil
}

func PermsEqual(a, b uint32) bool {
	switch runtime.GOOS {
	case "windows":
		// There is only writeable and read only, represented for user, group
		// and other equally. We only compare against user.
		return a&0600 == b&0600
	default:
		// All bits count
		return a&0777 == b&0777
	}
}

// A byteCounter gets bytes added to it via Update() and then provides the
// Total() and one minute moving average Rate() in bytes per second.
type byteCounter struct {
	total int64
	metrics.EWMA
	stop chan struct{}
}

func newByteCounter() *byteCounter {
	c := &byteCounter{
		EWMA: metrics.NewEWMA1(), // a one minute exponentially weighted moving average
		stop: make(chan struct{}),
	}
	go c.ticker()
	return c
}

func (c *byteCounter) ticker() {
	// The metrics.EWMA expects clock ticks every five seconds in order to
	// decay the average properly.
	t := time.NewTicker(5 * time.Second)
	for {
		select {
		case <-t.C:
			c.Tick()
		case <-c.stop:
			t.Stop()
			return
		}
	}
}

func (c *byteCounter) Update(bytes int64) {
	atomic.AddInt64(&c.total, bytes)
	c.EWMA.Update(bytes)
}

func (c *byteCounter) Total() int64 {
	return atomic.LoadInt64(&c.total)
}

func (c *byteCounter) Close() {
	close(c.stop)
}

// A no-op CurrentFiler

type noCurrentFiler struct{}

func (noCurrentFiler) CurrentFile(name string) (protocol.FileInfo, bool) {
	return protocol.FileInfo{}, false
}

// A no-op Lstater

type defaultLstater struct{}

func (defaultLstater) Lstat(name string) (os.FileInfo, error) {
	return osutil.Lstat(name)
}
