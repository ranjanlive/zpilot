package session

import "testing"

// Keep a large immutable transcript but render only the post-compaction tail.
func BenchmarkContextSnapshot(b *testing.B) {
	s := &Store{}
	for range 50000 {
		s.events = append(s.events, Event{Kind: KindStatus, Text: "retained transcript event"})
	}
	s.events = append(s.events, Event{Kind: KindCompaction, Compaction: &Compaction{Summary: "summary"}})
	for range 20 {
		s.events = append(s.events, Event{Kind: KindStatus, Text: "recent event"})
	}
	for i, ev := range s.events {
		s.indexEvent(ev, i)
	}
	b.Run("full_transcript", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if len(s.Events()) == 0 {
				b.Fatal("missing transcript")
			}
		}
	})
	b.Run("active_context", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if len(s.ContextEvents()) == 0 {
				b.Fatal("missing context")
			}
		}
	})
}
