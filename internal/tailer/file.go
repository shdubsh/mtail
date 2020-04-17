// Copyright 2018 Google Inc. All Rights Reserved.
// This file is available under the Apache license.

package tailer

import (
	"bytes"
	"expvar"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/golang/glog"
	"github.com/google/mtail/internal/logline"
	"github.com/pkg/errors"
)

var (
	// logErrors counts the number of IO errors per log file
	logErrors = expvar.NewMap("log_errors_total")
	// logRotations counts the number of rotations per log file
	logRotations = expvar.NewMap("log_rotations_total")
	// logTruncs counts the number of log truncation events per log
	logTruncs = expvar.NewMap("log_truncates_total")
	// lineCount counts the numbre of lines read per log file
	lineCount = expvar.NewMap("log_lines_total")
)

// File provides an abstraction over files and named pipes being tailed
// by `mtail`.
type File struct {
	Name     string    // Given name for the file (possibly relative, used for displau)
	Pathname string    // Full absolute path of the file used internally
	LastRead time.Time // time of the last read received on this handle
	regular  bool      // Remember if this is a regular file (or a pipe)
	file     *os.File
	partial  *bytes.Buffer
	lock      bool  // gate to prevent watcher event from creating a race condition writing and resetting partial
	lines    chan<- *logline.LogLine // output channel for lines read
}

// NewFile returns a new File named by the given pathname.  `seenBefore` indicates
// that mtail believes it's seen this pathname before, indicating we should
// retry on error to open the file. `seekToStart` indicates that the file
// should be tailed from offset 0, not EOF; the latter is true for rotated
// files and for files opened when mtail is in oneshot mode.
func NewFile(pathname string, lines chan<- *logline.LogLine, seekToStart bool) (*File, error) {
	glog.V(2).Infof("file.New(%s, %v)", pathname, seekToStart)
	absPath, err := filepath.Abs(pathname)
	if err != nil {
		return nil, err
	}
	f, err := open(absPath, false)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		// Stat failed, log error and return.
		logErrors.Add(absPath, 1)
		return nil, errors.Wrapf(err, "Failed to stat %q", absPath)
	}
	regular := false
	switch m := fi.Mode(); {
	case m.IsRegular():
		regular = true
		seekWhence := io.SeekEnd
		if seekToStart {
			seekWhence = io.SeekCurrent
		}
		if _, err := f.Seek(0, seekWhence); err != nil {
			return nil, errors.Wrapf(err, "Seek failed on %q", absPath)
		}
		// Named pipes are the same as far as we're concerned, but we can't seek them.
		fallthrough
	case m&os.ModeType == os.ModeNamedPipe:
	default:
		return nil, errors.Errorf("Can't open files with mode %v: %s", m&os.ModeType, absPath)
	}
	return &File{pathname, absPath, time.Now(), regular, f, bytes.NewBufferString(""), false, lines}, nil
}

func open(pathname string, seenBefore bool) (*os.File, error) {
	retries := 3
	retryDelay := 1 * time.Millisecond
	shouldRetry := func() bool {
		// seenBefore indicates also that we're rotating a file that previously worked, so retry.
		if !seenBefore {
			return false
		}
		return retries > 0
	}
	var f *os.File
Retry:
	// TODO(jaq): Can we avoid the NONBLOCK open on fifos with a goroutine per file?
	f, err := os.OpenFile(pathname, os.O_RDONLY|syscall.O_NONBLOCK, 0600)
	if err != nil {
		logErrors.Add(pathname, 1)
		if shouldRetry() {
			retries--
			time.Sleep(retryDelay)
			retryDelay += retryDelay
			goto Retry
		}
	}
	if err != nil {
		glog.Infof("open failed all retries")
		return nil, err
	}
	glog.V(2).Infof("open succeeded %s", pathname)
	return f, nil
}

// Follow reads from the file until EOF.  It tracks log rotations (i.e new inode or device).
func (f *File) Follow() error {
	s1, err := f.file.Stat()
	if err != nil {
		glog.V(1).Infof("Stat failed on %q: %s", f.Name, err)
		// We have a fd but it's invalid, handle as a rotation (delete/create)
		err := f.doRotation()
		if err != nil {
			return err
		}
	}
	s2, err := os.Stat(f.Pathname)
	if err != nil {
		glog.Infof("Stat failed on %q: %s", f.Pathname, err)
		return nil
	}
	if !os.SameFile(s1, s2) {
		glog.V(1).Infof("New inode detected for %s, treating as rotation", f.Pathname)
		err = f.doRotation()
		if err != nil {
			return err
		}
	} else {
		glog.V(1).Infof("Path %s already being watched, and inode not changed.",
			f.Pathname)
	}
	if !f.lock {
		glog.V(2).Info("doing the normal read")
		return f.Read()
	}
	return nil
}

// doRotation reads the remaining content of the currently opened file, then reopens the new one.
func (f *File) doRotation() error {
	glog.V(2).Info("doing the rotation flush read")
	if err := f.Read(); err != nil {
		glog.Info(err)
	}
	logRotations.Add(f.Name, 1)
	newFile, err := open(f.Pathname, true /*seenBefore*/)
	if err != nil {
		return err
	}
	f.file = newFile
	return nil
}

// Read blocks of 4096 bytes from the File, sending LogLines to the given
// channel as newlines are encountered.  If EOF is read, the partial line is
// stored to be concatenated to on the next call.  At EOF, checks for
// truncation and resets the file offset if so.
func (f *File) Read() error {
	b := make([]byte, 0, 4096)
	totalBytes := 0
	for {
		f.lock = true
		if err := f.file.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			glog.V(2).Infof("%s: %s", f.Name, err)
		}
		n, err := f.file.Read(b[:cap(b)])
		glog.V(2).Infof("Read count %v err %v", n, err)
		totalBytes += n
		b = b[:n]

		// If this time we've read no bytes at all and then hit an EOF, and
		// we're a regular file, check for truncation.
		if err == io.EOF && totalBytes == 0 && f.regular {
			glog.V(2).Info("Suspected truncation.")
			truncated, terr := f.checkForTruncate()
			if terr != nil {
				glog.Infof("checkForTruncate returned with error '%v'", terr)
			}
			if truncated {
				// Try again: offset was greater than filesize and now we've seeked to start.
				continue
			}
		}

		var (
			rune  rune
			width int
		)
		for i := 0; i < len(b) && i < n; i += width {
			rune, width = utf8.DecodeRune(b[i:])
			switch {
			case rune != '\n':
				f.waitForLock()
				f.partial.WriteRune(rune)
			default:
				f.sendLine()
			}
		}
		f.lock = false
		// Return on any error, including EOF.
		if err != nil {
			// Update the last read time if we were able to read anything.
			if totalBytes > 0 {
				f.LastRead = time.Now()
			}
			return err
		}
	}
}

func (f *File) waitForLock() {
	if f.partialLock {
		time.Sleep(1 * time.Millisecond)
	}
}

// sendLine sends the contents of the partial buffer off for processing.
func (f *File) sendLine() {
	f.partialLock = true
	f.lines <- logline.NewLogLine(f.Name, f.partial.String())
	lineCount.Add(f.Name, 1)
	glog.V(2).Info("Line sent")
	// reset partial accumulator
	f.partial.Reset()
	f.partialLock = false
}

// checkForTruncate checks to see if the current offset into the file
// is past the end of the file based on its size, and if so seeks to
// the start again.
func (f *File) checkForTruncate() (bool, error) {
	currentOffset, err := f.file.Seek(0, io.SeekCurrent)
	glog.V(2).Infof("current seek position at %d", currentOffset)
	if err != nil {
		return false, err
	}

	fi, err := f.file.Stat()
	if err != nil {
		return false, err
	}

	glog.V(2).Infof("File size is %d", fi.Size())
	if currentOffset == 0 || fi.Size() >= currentOffset {
		glog.V(2).Info("no truncate appears to have occurred")
		return false, nil
	}

	// We're about to lose all data because of the truncate so if there's
	// anything in the buffer, send it out.
	if f.partial.Len() > 0 {
		f.sendLine()
	}

	p, serr := f.file.Seek(0, io.SeekStart)
	glog.V(2).Infof("Truncated?  Seeked to %d: %v", p, serr)
	logTruncs.Add(f.Name, 1)
	return true, serr
}

func (f *File) Stat() (os.FileInfo, error) {
	return f.file.Stat()
}

func (f *File) Close() error {
	if f.partial.Len() > 0 {
		f.sendLine()
	}
	return f.file.Close()
}
