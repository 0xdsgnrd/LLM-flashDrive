package main

import "testing"

// Regenerate replaces an answer rather than appending a second one, so the old
// answer has to stop existing — otherwise a reload shows both attempts stacked.
func TestDropLast(t *testing.T) {
	s := newTestStore(t)
	meta, _ := s.Create("m")
	s.Append(meta.ID, Msg{Role: "user", Content: "the question"})
	s.Append(meta.ID, Msg{Role: "assistant", Content: "first attempt"})

	if err := s.DropLast(meta.ID); err != nil {
		t.Fatalf("DropLast: %v", err)
	}
	c, err := s.Get(meta.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(c.Messages) != 1 || c.Messages[0].Content != "the question" {
		t.Fatalf("wrong message survived: %+v", c.Messages)
	}
	// Model and title come from the header line, which truncation must not touch.
	if c.Model != "m" || c.Title != "the question" {
		t.Errorf("header damaged: model=%q title=%q", c.Model, c.Title)
	}
	// Appending the replacement answer has to still work afterwards.
	if err := s.Append(meta.ID, Msg{Role: "assistant", Content: "second attempt"}); err != nil {
		t.Fatalf("Append after drop: %v", err)
	}
	c, _ = s.Get(meta.ID)
	if len(c.Messages) != 2 || c.Messages[1].Content != "second attempt" {
		t.Fatalf("regenerated answer not stored: %+v", c.Messages)
	}
}

func TestDropLastRefusesToEatTheHeader(t *testing.T) {
	s := newTestStore(t)
	meta, _ := s.Create("m")
	if err := s.DropLast(meta.ID); err == nil {
		t.Error("dropped from a conversation that has no messages")
	}
	if _, err := s.Get(meta.ID); err != nil {
		t.Errorf("conversation damaged: %v", err)
	}
}

// A torn final line and a dropped line must not be confused: dropping after a
// crash should remove the last INTACT message, not resurrect the torn one.
func TestDropLastAfterATornWrite(t *testing.T) {
	s := newTestStore(t)
	meta, _ := s.Create("m")
	s.Append(meta.ID, Msg{Role: "user", Content: "question"})
	s.Append(meta.ID, Msg{Role: "assistant", Content: "answer"})
	if err := s.DropLast(meta.ID); err != nil {
		t.Fatalf("DropLast: %v", err)
	}
	c, _ := s.Get(meta.ID)
	if len(c.Messages) != 1 || c.Messages[0].Role != "user" {
		t.Fatalf("expected only the question to remain: %+v", c.Messages)
	}
}
