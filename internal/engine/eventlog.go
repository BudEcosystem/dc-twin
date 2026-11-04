package engine

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The committed log on disk (D27): the active file (events.jsonl) and its closed segments
// (events.<first seq>-<last seq>.jsonl, and the events.<time>.jsonl an estate restart leaves),
// kept within a total size and an age. The active file rolls into a segment when it reaches the
// segment size; closed segments are removed oldest first while the log is over its size or a
// segment is past its age, and the removal is itself committed, so the log says what it no
// longer holds. A stack that polls (NICo reads every BMC's inventory, firmware and health) commits
// tens of gigabytes of requests a day on a rack-scale estate, so a log kept whole fills a disk.

// LogRetention bounds the committed log on disk. Zero fields take the defaults.
type LogRetention struct {
	SegmentBytes int64         // the active file rolls into a segment at this size
	MaxBytes     int64         // the log's total, segments and active file
	MaxAge       time.Duration // a closed segment older than this is removed
}

// DefaultLogRetention is 256 MiB segments, 2 GiB in all, a week.
var DefaultLogRetention = LogRetention{SegmentBytes: 256 << 20, MaxBytes: 2 << 30, MaxAge: 7 * 24 * time.Hour}

func (r LogRetention) withDefaults() LogRetention {
	if r.SegmentBytes <= 0 {
		r.SegmentBytes = DefaultLogRetention.SegmentBytes
	}
	if r.MaxBytes <= 0 {
		r.MaxBytes = DefaultLogRetention.MaxBytes
	}
	if r.MaxAge <= 0 {
		r.MaxAge = DefaultLogRetention.MaxAge
	}
	if r.SegmentBytes > r.MaxBytes {
		r.SegmentBytes = r.MaxBytes
	}
	return r
}

type eventLog struct {
	path     string
	fh       *os.File
	w        *bufio.Writer
	size     int64
	firstSeq uint64
	lastSeq  uint64
	ret      LogRetention
	now      func() time.Time
}

func openEventLog(path string, ret LogRetention) (*eventLog, error) {
	fh, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	return &eventLog{path: path, fh: fh, w: bufio.NewWriter(fh), ret: ret.withDefaults(), now: time.Now}, nil
}

// write appends one encoded event; it reports the segments removed when the active file rolled.
func (l *eventLog) write(b []byte, seq uint64, flush bool) (removed []string) {
	if l.w == nil && !l.open() {
		return nil // the active file could not be reopened at the last roll; tried again next write
	}
	if l.firstSeq == 0 {
		l.firstSeq = seq
	}
	l.lastSeq = seq
	l.w.Write(b)
	l.w.WriteByte('\n')
	l.size += int64(len(b)) + 1
	if flush {
		l.w.Flush()
	}
	if l.size >= l.ret.SegmentBytes {
		return l.roll()
	}
	return nil
}

// roll closes the active file into a segment, starts a new one, and enforces the retention.
// A rename that fails leaves the active file as it is, written on and rolled a segment later.
func (l *eventLog) roll() []string {
	l.w.Flush()
	seg := segmentName(l.path, l.firstSeq, l.lastSeq)
	if err := os.Rename(l.path, seg); err != nil {
		l.size = 0
		return nil
	}
	l.fh.Close()
	l.fh, l.w, l.size, l.firstSeq = nil, nil, 0, 0
	l.open()
	return l.prune()
}

// open starts the active file after a roll (the old one was renamed, so it appends to nothing).
func (l *eventLog) open() bool {
	fh, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return false
	}
	l.fh, l.w = fh, bufio.NewWriter(fh)
	return true
}

// segmentName is a closed segment's file, by the sequence numbers it holds (zero-padded, so the
// names sort in order).
func segmentName(active string, first, last uint64) string {
	base := strings.TrimSuffix(active, ".jsonl")
	return fmt.Sprintf("%s.%012d-%012d.jsonl", base, first, last)
}

// prune removes closed segments, oldest first, while the log is over its total or a segment is
// past its age.
func (l *eventLog) prune() []string {
	base := strings.TrimSuffix(filepath.Base(l.path), ".jsonl")
	entries, err := os.ReadDir(filepath.Dir(l.path))
	if err != nil {
		return nil
	}
	type seg struct {
		path string
		size int64
		mod  time.Time
	}
	var segs []seg
	total := l.size
	for _, e := range entries {
		n := e.Name()
		if n == filepath.Base(l.path) || !strings.HasPrefix(n, base+".") || !strings.HasSuffix(n, ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		segs = append(segs, seg{filepath.Join(filepath.Dir(l.path), n), info.Size(), info.ModTime()})
		total += info.Size()
	}
	// oldest first; segments of one tick by name (their sequence numbers, zero-padded)
	sort.Slice(segs, func(i, j int) bool {
		if !segs[i].mod.Equal(segs[j].mod) {
			return segs[i].mod.Before(segs[j].mod)
		}
		return segs[i].path < segs[j].path
	})
	var removed []string
	cutoff := l.now().Add(-l.ret.MaxAge)
	for _, s := range segs {
		if total <= l.ret.MaxBytes && !s.mod.Before(cutoff) {
			break
		}
		if os.Remove(s.path) == nil {
			total -= s.size
			removed = append(removed, filepath.Base(s.path))
		}
	}
	return removed
}

func (l *eventLog) close() {
	if l.w != nil {
		l.w.Flush()
		l.fh.Close()
		l.w = nil
	}
}
