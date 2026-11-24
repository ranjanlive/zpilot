package auth

import "testing"

func TestPassword(t *testing.T) {
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("short password accepted")
	}
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !CheckPassword(h, "correct horse battery") || CheckPassword(h, "wrong horse battery") || CheckPassword("garbage", "x") {
		t.Fatal("password check wrong")
	}
	h2, _ := HashPassword("correct horse battery")
	if h == h2 {
		t.Fatal("hashes are not salted")
	}
}

func TestLimiter(t *testing.T) {
	l := NewLimiter()
	for i := 0; i < 5; i++ {
		if ok, _ := l.Allow("1.2.3.4"); !ok {
			t.Fatalf("blocked too early at attempt %d", i)
		}
		l.Result("1.2.3.4", false)
	}
	if ok, _ := l.Allow("1.2.3.4"); ok {
		t.Fatal("not blocked after 5 failures")
	}
	if ok, _ := l.Allow("5.6.7.8"); !ok {
		t.Fatal("other address blocked")
	}
}

func TestSessions(t *testing.T) {
	s := NewSessions()
	id := s.New("u1")
	if !s.Valid(id) || s.Valid("nope") || s.Valid("") {
		t.Fatal("session validity wrong")
	}
	s.End(id)
	if s.Valid(id) {
		t.Fatal("ended session still valid")
	}
}
