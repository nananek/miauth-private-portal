package rss

import "testing"

func TestHostFromFeedURL(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		{"https://Note.com/rss", "note.com"},
		{"https://blog.example.net:8443/feed.xml", "blog.example.net"},
		{"http://sub.domain.example/atom.xml?x=1", "sub.domain.example"},
	}
	for _, tc := range cases {
		got, err := HostFromFeedURL(tc.url)
		if err != nil {
			t.Fatalf("HostFromFeedURL(%q): %v", tc.url, err)
		}
		if got != tc.want {
			t.Errorf("HostFromFeedURL(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

func TestHostFromFeedURL_RejectsHostless(t *testing.T) {
	if _, err := HostFromFeedURL("not-a-url"); err == nil {
		t.Error("expected an error for a URL with no host")
	}
}

func TestDefaultUsername_NormalizesHost(t *testing.T) {
	got := DefaultUsername("Note.com", "https://note.com/rss", func(string) bool { return false })
	if got != "note_com" {
		t.Errorf("DefaultUsername = %q, want note_com", got)
	}
}

func TestDefaultUsername_DisambiguatesOnCollision(t *testing.T) {
	reserved := map[string]bool{"note_com": true}
	got := DefaultUsername("note.com", "https://note.com/user/blogb/rss", func(c string) bool { return reserved[c] })
	if got == "note_com" {
		t.Error("DefaultUsername returned the already-reserved candidate")
	}
	if len(got) == 0 || len(got) > usernameMaxLen {
		t.Errorf("DefaultUsername = %q, want a non-empty candidate within usernameMaxLen", got)
	}
	// Deterministic: the same inputs always produce the same suffix.
	got2 := DefaultUsername("note.com", "https://note.com/user/blogb/rss", func(c string) bool { return reserved[c] })
	if got != got2 {
		t.Errorf("DefaultUsername is not deterministic: %q vs %q", got, got2)
	}
}

func TestDefaultUsername_BoundedLength(t *testing.T) {
	longHost := "a-very-long-subdomain-that-exceeds-the-username-length-limit.example.com"
	got := DefaultUsername(longHost, "https://"+longHost+"/rss", func(string) bool { return false })
	if len(got) > usernameMaxLen {
		t.Errorf("DefaultUsername length = %d, want <= %d", len(got), usernameMaxLen)
	}
}
