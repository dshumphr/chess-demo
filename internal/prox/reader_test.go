package prox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func quiet(string, ...any) {}

func rec(id string) []byte {
	return []byte(`{"v":1,"id":"` + id + `","ts":"2026-11-20T00:00:00.000Z","src":"agent:x","type":"chess.move","body":{"san":"e5"}}`)
}

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendFile(t *testing.T, path string, data []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
}

func ids(recs [][]byte) []string {
	var out []string
	for _, r := range recs {
		var ev Event
		if err := json.Unmarshal(r, &ev); err != nil {
			panic(err)
		}
		out = append(out, ev.ID)
	}
	return out
}

func poll(t *testing.T, r *Reader) [][]byte {
	t.Helper()
	recs, err := r.Poll()
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func TestReaderSkipMalformedAndEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	write(t, path, []byte{})
	r, err := NewReader(path, quiet)
	if err != nil {
		t.Fatal(err)
	}
	// Reader baselines at EOF: existing records are never replayed.
	appendFile(t, path, []byte("\nnot json\n"+string(rec("a"))+"\n"))
	got := ids(poll(t, r))
	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("got %v, want [a]", got)
	}
	if poll(t, r) != nil {
		t.Fatal("expected no new records")
	}
}

func TestReaderDeliversTornPrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	write(t, path, rec("a"))
	r, err := NewReader(path, quiet)
	if err != nil {
		t.Fatal(err)
	}
	// Torn write: half a record, no trailing newline, doesn't parse yet.
	full := rec("b")
	appendFile(t, path, []byte("\n"+string(full[:len(full)/2])))
	if got := poll(t, r); len(got) != 0 {
		t.Fatalf("torn prefix delivered early: %v", got)
	}
	appendFile(t, path, full[len(full)/2:])
	got := ids(poll(t, r))
	if len(got) != 1 || got[0] != "b" {
		t.Fatalf("got %v, want [b]", got)
	}
}

func TestReaderDeliversFinalUnterminatedRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	write(t, path, rec("a"))
	r, err := NewReader(path, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if got := poll(t, r); len(got) != 0 {
		t.Fatalf("baseline not at EOF: %v", got)
	}
	appendFile(t, path, []byte("\n"+string(rec("b")))) // never terminated
	got := ids(poll(t, r))
	if len(got) != 1 || got[0] != "b" {
		t.Fatalf("got %v, want [b]", got)
	}
}

func TestReaderRebaselinesOnReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	write(t, path, rec("a"))
	r, err := NewReader(path, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if poll(t, r) != nil {
		t.Fatal("baseline not at EOF")
	}
	// proxenos rm + new: same path, fresh file from byte 0.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	write(t, path, rec("z"))
	got := ids(poll(t, r))
	if len(got) != 1 || got[0] != "z" {
		t.Fatalf("got %v, want [z] after replacement", got)
	}
}

func TestPollLoopOrderedUntilStop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	write(t, path, []byte{})
	r, err := NewReader(path, quiet)
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan []byte)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		r.PollLoop(out, 5*time.Millisecond, stop)
		close(done)
	}()
	appendFile(t, path, []byte("\n"+string(rec("a"))+"\n"+string(rec("b"))+"\n"))
	var got []string
	deadline := time.After(5 * time.Second)
	for len(got) < 2 {
		select {
		case rec := <-out:
			var ev Event
			if err := json.Unmarshal(rec, &ev); err != nil {
				t.Fatal(err)
			}
			got = append(got, ev.ID)
		case <-deadline:
			t.Fatalf("timeout, got %v", got)
		}
	}
	if got[0] != "a" || got[1] != "b" {
		t.Fatalf("out of order: %v", got)
	}
	close(stop)
	<-done
}
