package session

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const metaVersion = 1

var validID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ValidateID reports whether name is usable as a session ID (it becomes a
// directory name, so path separators and odd characters are refused).
func ValidateID(name string) error {
	if !validID.MatchString(name) {
		return fmt.Errorf("invalid session name %q: use letters, digits, '.', '_' or '-' (max 128, must start with a letter or digit)", name)
	}
	return nil
}

// NewID returns a sortable, human-readable unique ID: 20261011-195804-a1b2.
func NewID() string {
	var r [2]byte
	_, _ = rand.Read(r[:])
	return time.Now().Format("20060102-150405") + "-" + hex.EncodeToString(r[:])
}

// Store is one session's directory.
type Store struct {
	Dir string

	mu           sync.Mutex
	f            *os.File
	events       []Event
	notify       chan struct{} // closed and replaced on every append
	meta         Meta
	syncEach     bool
	contextStart int
	latestSystem Event
	turn         int
	pruned       map[string]bool
	pending      map[int64]Event
}

// Create makes a new session directory. It fails if the session exists.
func Create(dir string, meta Meta) (*Store, error) {
	if _, err := os.Stat(filepath.Join(dir, "events.jsonl")); err == nil {
		return nil, fmt.Errorf("session %s already exists", meta.ID)
	}
	if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0o755); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	meta.CreatedAt, meta.UpdatedAt, meta.Version = now, now, metaVersion
	s := &Store{Dir: dir, meta: meta, notify: make(chan struct{}), syncEach: true}
	if err := s.writeMeta(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	s.f = f
	return s, nil
}

// Open loads an existing session. A torn final line (a crash mid-write) is
// dropped and the file truncated back to the last complete event.
func Open(dir string) (*Store, error) {
	path := filepath.Join(dir, "events.jsonl")
	input, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("session not found: %s", filepath.Base(dir))
		}
		return nil, err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return nil, err
	}
	s := &Store{Dir: dir, notify: make(chan struct{}), syncEach: true}
	var good int64
	normalizeTail := false
	br := bufio.NewReader(input)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) > 0 {
				var ev Event
				if jerr := json.Unmarshal(trimmed, &ev); jerr != nil {
					if errors.Is(err, io.EOF) { // torn tail: stop here
						break
					}
					return nil, fmt.Errorf("%s: corrupt event at byte %d: %w", path, good, jerr)
				}
				s.events = append(s.events, ev)
			}
			good += int64(len(line))
			normalizeTail = errors.Is(err, io.EOF)
		}
		if err != nil {
			break
		}
	}
	if good < info.Size() {
		if err := os.Truncate(path, good); err != nil {
			return nil, fmt.Errorf("repair torn tail of %s: %w", path, err)
		}
	}
	for i := range s.events {
		s.events[i].Seq = int64(i + 1)
		s.indexEvent(s.events[i], i)
	}
	if mb, err := os.ReadFile(filepath.Join(dir, "meta.json")); err == nil {
		_ = json.Unmarshal(mb, &s.meta)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	if normalizeTail {
		if _, err := f.Write([]byte{'\n'}); err != nil {
			f.Close()
			return nil, err
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return nil, err
		}
	}
	s.f = f
	return s, nil
}

// Close flushes and closes the log.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	metaErr := s.writeMetaLocked()
	err := s.f.Close()
	s.f = nil
	return errors.Join(metaErr, err)
}

// Meta returns a copy of the session metadata.
func (s *Store) Meta() Meta {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.meta
}

// UpdateMeta applies fn to the metadata and persists it.
func (s *Store) UpdateMeta(fn func(*Meta)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := s.meta
	fn(&s.meta)
	if err := s.writeMetaLocked(); err != nil {
		s.meta = before
		return err
	}
	return nil
}

func (s *Store) writeMeta() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeMetaLocked()
}

func (s *Store) writeMetaLocked() error {
	s.meta.UpdatedAt = time.Now().UTC()
	b, err := json.MarshalIndent(s.meta, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(s.Dir, "meta.json"), b)
}

func atomicWrite(path string, data []byte) error { return atomicWritePerm(path, data, 0o644) }

func atomicWritePerm(path string, data []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".zagent-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(perm); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(tmp, path)
}

// Append writes an event, assigns its sequence number and wakes subscribers.
func (s *Store) Append(ev Event) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return ev, errors.New("session is closed")
	}
	ev.Seq = int64(len(s.events) + 1)
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return ev, err
	}
	if _, err := s.f.Write(append(line, '\n')); err != nil {
		return ev, fmt.Errorf("write event: %w", err)
	}
	if s.syncEach && ev.Kind != KindUsage {
		if err := s.f.Sync(); err != nil {
			return ev, fmt.Errorf("sync event log: %w", err)
		}
	}
	s.events = append(s.events, ev)
	s.indexEvent(ev, len(s.events)-1)
	close(s.notify)
	s.notify = make(chan struct{})
	return ev, nil
}

// Events returns a snapshot of all events.
func (s *Store) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, len(s.events))
	copy(out, s.events)
	return out
}

// ContextEvents excludes history before the latest compaction while preserving
// the current prompt, turn counter and global pruning policy. Full transcript
// replay continues to use Events/Since; compaction never deletes user history.
func (s *Store) ContextEvents() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, 0, 3+len(s.events)-s.contextStart)
	if s.latestSystem.Kind != "" {
		out = append(out, s.latestSystem)
	}
	out = append(out, Event{Kind: KindMessage, TurnNo: s.turn})
	if len(s.pruned) > 0 {
		blobs := make([]string, 0, len(s.pruned))
		for blob := range s.pruned {
			blobs = append(blobs, blob)
		}
		sort.Strings(blobs)
		out = append(out, Event{Kind: KindPrune, Blobs: blobs})
	}
	return append(out, s.events[s.contextStart:]...)
}

// PendingNotes returns only notes not yet consumed, in durable sequence order.
func (s *Store) PendingNotes() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, 0, len(s.pending))
	for _, ev := range s.pending {
		out = append(out, ev)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

func (s *Store) indexEvent(ev Event, index int) {
	if s.pending == nil {
		s.pending = map[int64]Event{}
	}
	for _, seq := range ev.Consumed {
		delete(s.pending, seq)
	}
	switch ev.Kind {
	case KindSystem:
		s.latestSystem = ev
	case KindMessage:
		if ev.TurnNo > s.turn {
			s.turn = ev.TurnNo
		}
	case KindCompaction:
		s.contextStart = index
	case KindPrune:
		if s.pruned == nil {
			s.pruned = map[string]bool{}
		}
		for _, blob := range ev.Blobs {
			s.pruned[blob] = true
		}
	case KindNote:
		s.pending[ev.Seq] = ev
	}
}

// Since returns events with Seq greater than seq.
func (s *Store) Since(seq int64) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	if seq < 0 {
		seq = 0
	}
	if int(seq) >= len(s.events) {
		return nil
	}
	out := make([]Event, len(s.events)-int(seq))
	copy(out, s.events[seq:])
	return out
}

// Len returns the number of events.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

// Sub is a live cursor over the log. It never drops events: a slow reader
// simply catches up from the in-memory log.
type Sub struct {
	s   *Store
	idx int
}

// Subscribe returns a cursor positioned after fromSeq (0 = from the start).
func (s *Store) Subscribe(fromSeq int64) *Sub {
	if fromSeq < 0 {
		fromSeq = 0
	}
	return &Sub{s: s, idx: int(fromSeq)}
}

// Next blocks for the next event or until ctx is done.
func (c *Sub) Next(ctx context.Context) (Event, error) {
	for {
		c.s.mu.Lock()
		if c.idx < len(c.s.events) {
			ev := c.s.events[c.idx]
			c.idx++
			c.s.mu.Unlock()
			return ev, nil
		}
		wait := c.s.notify
		c.s.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return Event{}, ctx.Err()
		}
	}
}

// ---- blobs ----

// PutBlob stores a payload and returns its content address (hex SHA-256).
// Identical payloads are stored once.
func (s *Store) PutBlob(data []byte) (string, error) {
	sum := sha256.Sum256(data)
	id := hex.EncodeToString(sum[:])
	path := s.blobPath(id)
	if _, err := os.Stat(path); err == nil {
		return id, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := atomicWrite(path, data); err != nil {
		return "", err
	}
	return id, nil
}

// Load implements provider.BlobLoader.
func (s *Store) Load(id string) ([]byte, error) {
	if len(id) != 64 || strings.Trim(id, "0123456789abcdef") != "" {
		return nil, fmt.Errorf("invalid blob id %q", id)
	}
	return os.ReadFile(s.blobPath(id))
}

func (s *Store) blobPath(id string) string { return filepath.Join(s.Dir, "blobs", id[:2], id) }

// ---- listing ----

// List returns the metadata of every session under dir, newest first.
func List(dir string) ([]Meta, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []Meta
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name(), "meta.json"))
		if err != nil {
			continue
		}
		var m Meta
		if json.Unmarshal(b, &m) == nil {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}
