package leads

import (
	"reflect"
	"testing"
)

func TestNormalizeProfileURL(t *testing.T) {
	const want = "https://www.linkedin.com/in/jane-doe"
	same := []string{
		"https://www.linkedin.com/in/jane-doe",
		"https://www.linkedin.com/in/jane-doe/",
		"HTTPS://WWW.LINKEDIN.COM/IN/Jane-Doe",
		"http://linkedin.com/in/jane-doe",
		"linkedin.com/in/jane-doe",
		"www.linkedin.com/in/jane-doe/",
		"https://in.linkedin.com/in/jane-doe",
		"https://uk.linkedin.com/in/jane-doe?trk=public_profile",
		"https://www.linkedin.com/in/jane-doe/?originalSubdomain=in#top",
		"https://www.linkedin.com/in/jane-doe/details/experience/",
		"https://www.linkedin.com/in/jane-doe/overlay/contact-info/",
		"  https://www.linkedin.com/in/jane-doe  ",
		"https://www.linkedin.com:443/in/jane-doe",
		"https://www.linkedin.com./in/jane-doe",
	}
	for _, in := range same {
		got, err := NormalizeProfileURL(in)
		if err != nil || got != want {
			t.Errorf("%q -> %q, %v; want %q", in, got, err, want)
		}
	}
	// Hashed-style identifiers are valid slugs too, and unicode names survive.
	if got, err := NormalizeProfileURL("linkedin.com/in/ACoAABc123"); err != nil || got != "https://www.linkedin.com/in/acoaabc123" {
		t.Errorf("hashed: %q %v", got, err)
	}
	a, _ := NormalizeProfileURL("https://www.linkedin.com/in/jos%C3%A9-garc%C3%ADa")
	b, _ := NormalizeProfileURL("https://www.linkedin.com/in/josé-garcía/")
	if a == "" || a != b {
		t.Errorf("unicode forms differ: %q vs %q", a, b)
	}
}

func TestNormalizeProfileURLRejects(t *testing.T) {
	bad := []string{
		"",
		"   ",
		"jane-doe",
		"https://example.com/in/jane-doe",
		"https://linkedin.com.evil.net/in/jane-doe",
		"https://evil-linkedin.com/in/jane-doe",
		"https://notlinkedin.com/in/jane-doe",
		"https://user:pass@www.linkedin.com/in/jane-doe",
		"https://www.linkedin.com:8443/in/jane-doe",
		"ftp://www.linkedin.com/in/jane-doe",
		"javascript:alert(1)",
		"https://www.linkedin.com/company/acme",
		"https://www.linkedin.com/sales/lead/ACwAA",
		"https://www.linkedin.com/in/",
		"https://www.linkedin.com/in",
		"https://www.linkedin.com/in/..",
		"https://www.linkedin.com/in/-",
		"https://www.linkedin.com/in/a%20b",
		"https://www.linkedin.com/in/a%zz",
		"https://www.linkedin.com/in/" + string(make([]byte, 0)) + repeat("a", 101),
		"https://www.linkedin.com/" + repeat("a", 2100),
	}
	for _, in := range bad {
		if got, err := NormalizeProfileURL(in); err == nil {
			t.Errorf("%.60q accepted as %q", in, got)
		}
	}
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

func TestValidateSearchURL(t *testing.T) {
	ok := "https://www.linkedin.com/search/results/people/?keywords=cto&origin=FACETED_SEARCH&geoUrn=%5B%22102713980%22%5D"
	got, err := ValidateSearchURL(ok)
	if err != nil || got != ok {
		t.Fatalf("%q %v", got, err)
	}
	if got, err := ValidateSearchURL("linkedin.com/search/results/people?keywords=cto#frag"); err != nil || got != "https://www.linkedin.com/search/results/people?keywords=cto" {
		t.Fatalf("normalised: %q %v", got, err)
	}
	for _, bad := range []string{
		"", "https://example.com/search/results/people/?keywords=x",
		"https://www.linkedin.com/search/results/companies/?keywords=x",
		"https://www.linkedin.com/in/jane-doe",
		"https://linkedin.com.evil.net/search/results/people/",
	} {
		if _, err := ValidateSearchURL(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestSplitPasted(t *testing.T) {
	in := "https://a.com/1, https://a.com/2;https://a.com/3\r\nhttps://a.com/4\n\n  https://a.com/5\t"
	want := []string{"https://a.com/1", "https://a.com/2", "https://a.com/3", "https://a.com/4", "https://a.com/5"}
	if got := splitPasted(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	if len(splitPasted("  \n , ; ")) != 0 {
		t.Fatal("blank input should yield nothing")
	}
}
