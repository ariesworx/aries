package main

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
)

// browser stands in for the person at a browser: it follows the
// provider's pages, fills in the sign-in form and accepts a consent
// form, and stops at a redirect away from the provider, as an address
// bar would show it. It is not part of the design.
type browser struct {
	c          *http.Client
	provider   string // host:port of the provider; any other host is "away"
	user, pass string
	// follow says whether to fetch a redirect away from the provider:
	// yes when the browser is on the same machine as `aries login`
	// (loopback), no when it is on another (the paste path).
	follow bool
	// Pages counts the provider's pages seen, for the report.
	Pages []string
}

func newBrowser(provider, user, pass string, follow bool) *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{
		c: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		provider: provider, user: user, pass: pass, follow: follow,
	}
}

var (
	formRE  = regexp.MustCompile(`(?s)<form\b([^>]*)>(.*?)</form>`)
	attrRE  = regexp.MustCompile(`([a-zA-Z_-]+)="([^"]*)"`)
	inputRE = regexp.MustCompile(`(?s)<(?:input|button)\b([^>]*)>`)
	titleRE = regexp.MustCompile(`(?s)<title>(.*?)</title>`)
)

// open visits u and goes on until it lands away from the provider, or on
// a page with no form. It returns the address it stopped at and the last
// page's body.
func (b *browser) open(ctx context.Context, u string) (stoppedAt, body string, err error) {
	method, form := http.MethodGet, url.Values(nil)
	for range 20 {
		next, err := url.Parse(u)
		if err != nil {
			return "", "", err
		}
		if next.Scheme != "http" && next.Scheme != "https" || next.Host != b.provider {
			if !b.follow || next.Scheme != "http" {
				return u, "", nil
			}
		}
		var rd io.Reader
		if form != nil {
			rd = strings.NewReader(form.Encode())
		}
		req, err := http.NewRequestWithContext(ctx, method, u, rd)
		if err != nil {
			return "", "", err
		}
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		resp, err := b.c.Do(req)
		if err != nil {
			return "", "", err
		}
		page, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if loc := resp.Header.Get("Location"); resp.StatusCode/100 == 3 && loc != "" {
			ref, err := resp.Request.URL.Parse(loc)
			if err != nil {
				return "", "", err
			}
			u, method, form = ref.String(), http.MethodGet, nil
			continue
		}
		if next.Host != b.provider {
			return u, string(page), nil // the loopback listener's own page
		}
		if t := titleRE.FindSubmatch(page); t != nil {
			b.Pages = append(b.Pages, strings.TrimSpace(html.UnescapeString(string(t[1]))))
		}
		action, fields, ok := b.fill(string(page))
		if !ok {
			return u, string(page), nil
		}
		ref, err := resp.Request.URL.Parse(action)
		if err != nil {
			return "", "", err
		}
		u, method, form = ref.String(), http.MethodPost, fields
	}
	return "", "", errors.New("too many pages")
}

// fill picks the page's form and fills it: the sign-in form gets the
// credentials, a consent form is accepted, any other form is sent as it
// is (the device page with its code already in).
func (b *browser) fill(page string) (action string, fields url.Values, ok bool) {
	m := formRE.FindStringSubmatch(page)
	if m == nil {
		return "", nil, false
	}
	attrs := attrMap(m[1])
	if attrs["action"] == "" {
		return "", nil, false
	}
	fields = url.Values{}
	for _, in := range inputRE.FindAllStringSubmatch(m[2], -1) {
		a := attrMap(in[1])
		name := a["name"]
		if name == "" {
			continue
		}
		switch {
		case name == "username":
			fields.Set(name, b.user)
		case name == "password":
			fields.Set(name, b.pass)
		case name == "accept" || name == "cancel":
			// Consent: send accept only.
			if name == "accept" {
				fields.Set(name, a["value"])
			}
		case a["type"] == "submit" || a["type"] == "checkbox":
		default:
			fields.Set(name, a["value"])
		}
	}
	return attrs["action"], fields, true
}

func attrMap(s string) map[string]string {
	m := map[string]string{}
	for _, a := range attrRE.FindAllStringSubmatch(s, -1) {
		m[strings.ToLower(a[1])] = html.UnescapeString(a[2])
	}
	return m
}

func (b *browser) String() string { return fmt.Sprintf("pages %q", b.Pages) }
