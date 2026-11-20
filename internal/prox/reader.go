package prox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"syscall"
	"time"
)

// Reader polls a proxenos stream file directly and yields raw event
// records, applying the reader rules from proxenos DESIGN.md:
//
//   - skip empty lines;
//   - skip malformed terminated lines (log, never fatal);
//   - deliver a final unterminated record when it parses;
//   - hold a torn prefix (a trailing partial write) until it completes;
//   - re-baseline on file replacement (dev/ino change: the stream was
//     re-created — start from byte 0 of the new file).
//
// Framing: one record = one write(2) under O_APPEND, "\n"-prefixed JSON.
// Callers dedupe on event id; retries and re-attach can deliver a record twice.
type Reader struct {
	path   string
	offset int64
	dev    uint64
	ino    uint64
	carry  []byte // torn prefix held between polls
	logf   func(string, ...any)
}

// NewReader starts a reader at the current end of the file: on attach the
// app baselines at "now" and never replays history that predates it.
func NewReader(path string, logf func(string, ...any)) (*Reader, error) {
	r := &Reader{path: path, logf: logf}
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	r.dev, r.ino = devIno(st)
	r.offset = st.Size()
	return r, nil
}

// Poll reads any new bytes and returns the raw JSON of each complete,
// well-formed record, in order. It is safe to call concurrently with
// appends; a record being written mid-poll is simply held as a torn prefix.
func (r *Reader) Poll() ([][]byte, error) {
	st, err := os.Stat(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // stream removed; keep state, maybe recreated later
		}
		return nil, err
	}
	if dev, ino := devIno(st); dev != r.dev || ino != r.ino {
		// File replaced (`proxenos rm` + `proxenos new`): re-baseline at 0.
		r.dev, r.ino = dev, ino
		r.offset = 0
		r.carry = nil
	}
	if st.Size() <= r.offset {
		return nil, nil // nothing new (a held torn prefix stays held)
	}
	f, err := os.Open(r.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data := make([]byte, st.Size()-r.offset)
	if _, err := f.ReadAt(data, r.offset); err != nil {
		return nil, err
	}
	r.offset += int64(len(data))
	buf := data
	if r.carry != nil {
		// A torn prefix from a previous poll is already consumed
		// (offset is past it); the new bytes complete it.
		buf = append(r.carry, data...)
		r.carry = nil
	}

	var out [][]byte
	for {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			break
		}
		seg := bytes.TrimSpace(buf[:i])
		buf = buf[i+1:]
		if len(seg) == 0 {
			continue // empty line
		}
		if !isRecord(seg) {
			r.logf("skipping malformed record: %s", truncate(seg, 120))
			continue
		}
		out = append(out, append([]byte(nil), seg...))
	}
	// Trailing bytes without a newline: deliver them only if they already
	// parse as a complete record (the writer's final flush), else hold.
	if len(bytes.TrimSpace(buf)) > 0 {
		if isRecord(bytes.TrimSpace(buf)) {
			out = append(out, append([]byte(nil), bytes.TrimSpace(buf)...))
		} else {
			r.carry = append([]byte(nil), buf...)
		}
	}
	return out, nil
}

// isRecord reports whether seg decodes as a full event envelope.
func isRecord(seg []byte) bool {
	var ev Event
	return json.Unmarshal(seg, &ev) == nil && ev.ID != "" && ev.Type != ""
}

// PollLoop runs Poll on a ticker and pushes each raw record to the channel,
// in order, until stop is closed. It never touches app state directly.
func (r *Reader) PollLoop(out chan<- []byte, interval time.Duration, stop <-chan struct{}) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			recs, err := r.Poll()
			if err != nil {
				r.logf("poll error: %v", err)
				continue
			}
			for _, rec := range recs {
				select {
				case out <- rec:
				case <-stop:
					return
				}
			}
		}
	}
}

// devIno identifies the file instance for replacement detection. On Linux
// (the supported runtime) Sys() is *syscall.Stat_t; elsewhere we fall back
// to a size+mtime fingerprint, which still catches a fresh file.
func devIno(st os.FileInfo) (uint64, uint64) {
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return s.Dev, s.Ino
	}
	return uint64(st.ModTime().UnixNano()), uint64(st.Size())
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return fmt.Sprintf("%s…", b[:n])
	}
	return string(b)
}
