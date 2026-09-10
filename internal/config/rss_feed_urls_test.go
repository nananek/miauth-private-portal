package config

import "testing"

func strp(s string) *string { return &s }

func TestSplitRSSFeedURLs_ParsesURLsAndOptionalUsernames(t *testing.T) {
	cases := []struct {
		name          string
		value         string
		wantURLs      []string
		wantUsernames []*string
	}{
		{
			name:          "single entry no username",
			value:         "https://example.com/feed.xml",
			wantURLs:      []string{"https://example.com/feed.xml"},
			wantUsernames: []*string{nil},
		},
		{
			name:          "single entry with username",
			value:         "https://example.com/feed.xml|alice",
			wantURLs:      []string{"https://example.com/feed.xml"},
			wantUsernames: []*string{strp("alice")},
		},
		{
			name:  "multiple entries mixing both",
			value: "https://example.com/a.xml,https://example.com/b.xml|bob",
			wantURLs: []string{
				"https://example.com/a.xml",
				"https://example.com/b.xml",
			},
			wantUsernames: []*string{nil, strp("bob")},
		},
		{
			name:          "url containing a comma in its own query string",
			value:         "https://example.com/feed.xml?a=1,2|carol",
			wantURLs:      []string{"https://example.com/feed.xml?a=1,2"},
			wantUsernames: []*string{strp("carol")},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			urls, usernames := SplitRSSFeedURLs(tc.value)
			if len(urls) != len(tc.wantURLs) || len(usernames) != len(tc.wantUsernames) {
				t.Fatalf("SplitRSSFeedURLs(%q) = (%v, %v), want (%v, %v)", tc.value, urls, usernames, tc.wantURLs, tc.wantUsernames)
			}
			for i := range urls {
				if urls[i] != tc.wantURLs[i] {
					t.Errorf("urls[%d] = %q, want %q", i, urls[i], tc.wantURLs[i])
				}
				gotU, wantU := usernames[i], tc.wantUsernames[i]
				if (gotU == nil) != (wantU == nil) || (gotU != nil && *gotU != *wantU) {
					t.Errorf("usernames[%d] = %v, want %v", i, gotU, wantU)
				}
			}
		})
	}
}

func TestSplitRSSFeedURLs_EmptyValueReturnsNilNil(t *testing.T) {
	urls, usernames := SplitRSSFeedURLs("")
	if urls != nil || usernames != nil {
		t.Fatalf("SplitRSSFeedURLs(\"\") = (%v, %v), want (nil, nil)", urls, usernames)
	}
}

func TestJoinRSSFeedURLs_IsSplitRSSFeedURLsInverse(t *testing.T) {
	fixtures := []string{
		"https://example.com/feed.xml",
		"https://example.com/feed.xml|alice",
		"https://example.com/a.xml,https://example.com/b.xml|bob",
		"https://example.com/feed.xml?a=1,2|carol",
	}
	for _, v := range fixtures {
		t.Run(v, func(t *testing.T) {
			urls, usernames := SplitRSSFeedURLs(v)
			if got := JoinRSSFeedURLs(urls, usernames); got != v {
				t.Errorf("JoinRSSFeedURLs(SplitRSSFeedURLs(%q)) = %q, want %q", v, got, v)
			}
		})
	}
}

func TestJoinRSSFeedURLs_EmptyListsReturnsEmptyString(t *testing.T) {
	if got := JoinRSSFeedURLs(nil, nil); got != "" {
		t.Fatalf("JoinRSSFeedURLs(nil, nil) = %q, want \"\"", got)
	}
}
