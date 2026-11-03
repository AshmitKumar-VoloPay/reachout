// Package leads holds prospects: import from pasted profile URLs or a LinkedIn people
// search, de-duplication per user, listing and deletion.
package leads

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
	"unicode"
)

var (
	errNotLinkedIn = errors.New("not a linkedin.com URL")
	errNotProfile  = errors.New("not a LinkedIn profile URL (expected linkedin.com/in/<name>)")
	errBadSlug     = errors.New("profile name in the URL looks invalid")
)

var slugRe = regexp.MustCompile(`^[\p{L}\p{N}_.\-]{1,100}$`)

// isLinkedInHost accepts linkedin.com and its subdomains (www, in, uk, ...) and nothing
// that merely contains the word, such as evil-linkedin.com or linkedin.com.evil.net.
func isLinkedInHost(h string) bool {
	h = strings.TrimSuffix(strings.ToLower(h), ".")
	return h == "linkedin.com" || strings.HasSuffix(h, ".linkedin.com")
}

func parseLinkedInURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 2000 {
		return nil, errNotLinkedIn
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Hostname() == "" {
		return nil, errNotLinkedIn
	}
	if p := u.Port(); p != "" && p != "80" && p != "443" {
		return nil, errNotLinkedIn
	}
	if !isLinkedInHost(u.Hostname()) {
		return nil, errNotLinkedIn
	}
	return u, nil
}

// NormalizeProfileURL turns any form of a LinkedIn profile link into one canonical
// string, https://www.linkedin.com/in/<name>, so the same person is never stored twice.
// Query strings, fragments, trailing paths (/details/experience) and case are dropped.
func NormalizeProfileURL(raw string) (string, error) {
	u, err := parseLinkedInURL(raw)
	if err != nil {
		return "", err
	}
	segs := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	if len(segs) < 2 || !strings.EqualFold(segs[0], "in") {
		return "", errNotProfile
	}
	slug, err := url.PathUnescape(segs[1])
	if err != nil {
		return "", errBadSlug
	}
	slug = strings.ToLower(slug)
	if !slugRe.MatchString(slug) || strings.Trim(slug, ".-") == "" {
		return "", errBadSlug
	}
	for _, r := range slug {
		if unicode.IsControl(r) {
			return "", errBadSlug
		}
	}
	return "https://www.linkedin.com/in/" + url.PathEscape(slug), nil
}

// ValidateSearchURL checks a LinkedIn people-search link as copied from the browser.
// The URL is passed to Linked API as is, so only the host and path are enforced.
func ValidateSearchURL(raw string) (string, error) {
	u, err := parseLinkedInURL(raw)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(strings.TrimRight(u.Path, "/"), "/search/results/people") {
		return "", errors.New("expected a LinkedIn people search URL (linkedin.com/search/results/people/...)")
	}
	u.Scheme, u.Host, u.Fragment = "https", "www.linkedin.com", ""
	return u.String(), nil
}

// splitPasted breaks a pasted blob into candidate URLs. Whitespace, commas and
// semicolons all separate entries, so one-per-line, CSV cells and run-on text all work.
func splitPasted(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool {
		return unicode.IsSpace(r) || r == ',' || r == ';'
	})
}
