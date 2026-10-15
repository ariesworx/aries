package main

import (
	"context"
	"crypto/tls"
	"errors"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// browser stands in for the person at a browser, as spike/identity's
// does: it follows the pages of the Aries server and the provider, fills
// in the provider's sign-in form, and stops at a redirect to anywhere
// else, which it fetches only if it is loopback http (the browser is on
// the same machine as the client). It is not part of the design.
type browser struct {
	c          *http.Client
	hosts      []string // host:port of the server and the provider
	user, pass string
	Pages      []string
}

func newBrowser(tc *tls.Config, user, pass string, hosts ...string) *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{
		c: &http.Client{Jar: jar, Transport: &http.Transport{TLSClientConfig: tc},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		hosts: hosts, user: user, pass: pass,
	}
}

var (
	formRE  = regexp.MustCompile(`(?s)<form\b([^>]*)>(.*?)</form>`)
	attrRE  = regexp.MustCompile(`([a-zA-Z_-]+)="([^"]*)"`)
	inputRE = regexp.MustCompile(`(?s)<(?:input|button)\b([^>]*)>`)
	titleRE = regexp.MustCompile(`(?s)<title>(.*?)</title>`)
)

// open visits u and goes on until it lands off the known hosts, or on a
// page with no form. It returns where it stopped, the last status and
// the last page.
func (b *browser) open(ctx context.Context, u string) (stoppedAt string, status int, body string, err error) {
	method, form := http.MethodGet, url.Values(nil)
	for range 20 {
		next, err := url.Parse(u)
		if err != nil {
			return "", 0, "", err
		}
		known := slices.Contains(b.hosts, next.Host)
		if !known && !(next.Scheme == "http" && loopback(u)) {
			return u, 0, "", nil
		}
		var rd io.Reader
		if form != nil {
			rd = strings.NewReader(form.Encode())
		}
		req, err := http.NewRequestWithContext(ctx, method, u, rd)
		if err != nil {
			return "", 0, "", err
		}
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		resp, err := b.c.Do(req)
		if err != nil {
			return "", 0, "", err
		}
		page, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if loc := resp.Header.Get("Location"); resp.StatusCode/100 == 3 && loc != "" {
			ref, err := resp.Request.URL.Parse(loc)
			if err != nil {
				return "", 0, "", err
			}
			u, method, form = ref.String(), http.MethodGet, nil
			continue
		}
		if !known {
			return u, resp.StatusCode, string(page), nil // the loopback listener's own page
		}
		if t := titleRE.FindSubmatch(page); t != nil {
			b.Pages = append(b.Pages, strings.TrimSpace(html.UnescapeString(string(t[1]))))
		}
		action, fields, ok := b.fill(string(page))
		if !ok {
			return u, resp.StatusCode, string(page), nil
		}
		ref, err := resp.Request.URL.Parse(action)
		if err != nil {
			return "", 0, "", err
		}
		u, method, form = ref.String(), http.MethodPost, fields
	}
	return "", 0, "", errors.New("too many pages")
}

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
		switch {
		case name == "":
		case name == "username":
			fields.Set(name, b.user)
		case name == "password":
			fields.Set(name, b.pass)
		case name == "accept":
			fields.Set(name, a["value"])
		case name == "cancel" || a["type"] == "submit" || a["type"] == "checkbox":
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
