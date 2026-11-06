package automation

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"

	"golang.org/x/net/publicsuffix"
)

const (
	cookieSizeLimit    = 4096
	cookieAttributeMax = 1024
)

type jarReference struct {
	ID   string `json:"id"`
	Root string `json:"root"`
	Path string `json:"path"`
}

type storedCookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	HostOnly bool    `json:"hostOnly"`
	Path     string  `json:"path"`
	Expires  *int64  `json:"expires"`
	Secure   bool    `json:"secure"`
	HTTPOnly bool    `json:"httpOnly"`
	SameSite *string `json:"sameSite"`
}

func (c *storedCookie) expired(now time.Time) bool {
	return c.Expires != nil && *c.Expires <= now.UnixMilli()
}

type cookieStore struct {
	access  sync.Mutex
	cookies []storedCookie
}

type cookieJar struct {
	engine *engine
	memory *cookieStore
	file   jarReference
}

func (e *engine) cookieJar(reference jarReference) (*cookieJar, error) {
	if reference.ID != "" {
		e.access.Lock()
		defer e.access.Unlock()
		if e.jars == nil {
			e.jars = make(map[string]*cookieStore)
		}
		store := e.jars[reference.ID]
		if store == nil {
			store = &cookieStore{}
			e.jars[reference.ID] = store
		}
		return &cookieJar{engine: e, memory: store}, nil
	}
	at, err := e.fileLocation(reference.Root, reference.Path)
	if err != nil {
		return nil, err
	}
	_ = at.root.Close()
	return &cookieJar{engine: e, file: reference}, nil
}

func (j *cookieJar) update(change func(cookies []storedCookie) []storedCookie) error {
	if j.memory != nil {
		j.memory.access.Lock()
		defer j.memory.access.Unlock()
		j.memory.cookies = change(slices.Clone(j.memory.cookies))
		return nil
	}
	host := j.engine.host
	host.cookieAccess.Lock()
	defer host.cookieAccess.Unlock()
	at, err := j.engine.fileLocation(j.file.Root, j.file.Path)
	if err != nil {
		return err
	}
	defer at.root.Close()
	var cookies []storedCookie
	data, err := at.root.read(at.name)
	switch {
	case errors.Is(err, ErrNotFound):
	case err != nil:
		return err
	default:
		if err = json.Unmarshal(data, &cookies); err != nil {
			return E.Cause1(ErrInvalid, E.Cause(err, "read the cookies in ", at.root.where))
		}
		for index := range cookies {
			normalizeCookie(&cookies[index])
		}
	}
	changed := change(slices.Clone(cookies))
	if slices.Equal(changed, cookies) {
		return nil
	}
	return writeCookies(at, changed)
}

func (j *cookieJar) clear() error {
	if j.memory != nil {
		j.memory.access.Lock()
		defer j.memory.access.Unlock()
		j.memory.cookies = nil
		return nil
	}
	host := j.engine.host
	host.cookieAccess.Lock()
	defer host.cookieAccess.Unlock()
	at, err := j.engine.fileLocation(j.file.Root, j.file.Path)
	if err != nil {
		return err
	}
	defer at.root.Close()
	return writeCookies(at, nil)
}

func writeCookies(at *fileLocation, cookies []storedCookie) error {
	if cookies == nil {
		cookies = []storedCookie{}
	}
	data, err := json.Marshal(cookies)
	if err != nil {
		return err
	}
	return at.root.write(at.name, data)
}

func normalizeCookie(cookie *storedCookie) {
	cookie.Domain = strings.ToLower(strings.TrimPrefix(cookie.Domain, "."))
	if cookie.SameSite != nil {
		cookie.SameSite = sameSiteOf(*cookie.SameSite)
	}
}

func sameSiteOf(value string) *string {
	switch value = strings.ToLower(value); value {
	case "strict", "lax", "none":
		return &value
	}
	return nil
}

func secureURL(u *url.URL) bool {
	if u.Scheme == "https" || u.Scheme == "wss" {
		return true
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

func cookiePath(u *url.URL) string {
	path := u.EscapedPath()
	if path == "" {
		return "/"
	}
	return path
}

func (j *cookieJar) matching(u *url.URL) ([]storedCookie, error) {
	var out []storedCookie
	err := j.update(func(cookies []storedCookie) []storedCookie {
		now := time.Now()
		if u == nil {
			out = common.Filter(cookies, func(it storedCookie) bool { return !it.expired(now) })
			return cookies
		}
		host := strings.ToLower(u.Hostname())
		secure := secureURL(u)
		path := cookiePath(u)
		out = common.Filter(cookies, func(it storedCookie) bool {
			return !it.expired(now) && (it.Domain == host || !it.HostOnly && domainMatch(host, it.Domain)) && pathMatch(path, it.Path) && (secure || !it.Secure)
		})
		slices.SortStableFunc(out, func(a, b storedCookie) int { return len(b.Path) - len(a.Path) })
		return cookies
	})
	return out, err
}

func (j *cookieJar) header(u *url.URL) (string, error) {
	cookies, err := j.matching(u)
	if err != nil {
		return "", err
	}
	return strings.Join(common.Map(cookies, func(it storedCookie) string {
		if it.Name == "" {
			return it.Value
		}
		return it.Name + "=" + it.Value
	}), "; "), nil
}

func (j *cookieJar) store(u *url.URL, lines []string) error {
	if len(lines) == 0 {
		return nil
	}
	return j.update(func(cookies []storedCookie) []storedCookie {
		now := time.Now()
		for _, line := range lines {
			if cookie, err := parseSetCookie(line, u, now); err == nil {
				cookies, _ = storeCookie(cookies, u, cookie, now)
			}
		}
		return cookies
	})
}

func attachCookies(header map[string][]string, cookies string) {
	switch existing := strings.Join(header["Cookie"], "; "); {
	case cookies == "":
	case existing == "":
		header["Cookie"] = []string{cookies}
	default:
		header["Cookie"] = []string{existing + "; " + cookies}
	}
}

func parseSetCookie(line string, u *url.URL, now time.Time) (storedCookie, error) {
	if strings.ContainsFunc(line, func(character rune) bool { return character < 0x20 && character != '\t' || character == 0x7f }) {
		return storedCookie{}, E.New("the cookie has control characters")
	}
	pair, attributes, _ := strings.Cut(line, ";")
	name, value, found := strings.Cut(pair, "=")
	if !found {
		name, value = "", name
	}
	name, value = strings.Trim(name, " \t"), strings.Trim(value, " \t")
	switch {
	case name == "" && value == "":
		return storedCookie{}, E.New("the cookie has no name or value")
	case len(name)+len(value) > cookieSizeLimit:
		return storedCookie{}, E.New("the cookie is longer than ", cookieSizeLimit, " bytes")
	}
	cookie := storedCookie{Name: name, Value: value}
	var maxAge, expires *int64
	domain, path := "", ""
	for attribute := range strings.SplitSeq(attributes, ";") {
		key, attributeValue, _ := strings.Cut(attribute, "=")
		key, attributeValue = strings.ToLower(strings.Trim(key, " \t")), strings.Trim(attributeValue, " \t")
		if len(attributeValue) > cookieAttributeMax {
			continue
		}
		switch key {
		case "expires":
			if date, ok := parseCookieDate(attributeValue); ok {
				expires = new(date.UnixMilli())
			}
		case "max-age":
			if seconds, err := strconv.ParseInt(attributeValue, 10, 64); err == nil && attributeValue != "" && (attributeValue[0] == '-' || attributeValue[0] >= '0' && attributeValue[0] <= '9') {
				if seconds <= 0 {
					maxAge = new(int64(0))
				} else {
					maxAge = new(now.Add(time.Duration(min(seconds, int64(time.Duration(1<<62)/time.Second))) * time.Second).UnixMilli())
				}
			}
		case "domain":
			if attributeValue != "" {
				domain = strings.ToLower(strings.TrimPrefix(attributeValue, "."))
			}
		case "path":
			path = attributeValue
		case "secure":
			cookie.Secure = true
		case "httponly":
			cookie.HTTPOnly = true
		case "samesite":
			cookie.SameSite = sameSiteOf(attributeValue)
		}
	}
	switch {
	case maxAge != nil:
		cookie.Expires = maxAge
	case expires != nil:
		cookie.Expires = expires
	}
	host := strings.ToLower(u.Hostname())
	cookie.HostOnly, cookie.Domain = true, host
	if domain != "" {
		switch suffix, _ := publicsuffix.PublicSuffix(domain); {
		case net.ParseIP(host) != nil || suffix == domain:
			if domain != host {
				return storedCookie{}, E.New("the cookie's domain ", domain, " can't be set from ", host)
			}
		case !domainMatch(host, domain):
			return storedCookie{}, E.New("the cookie's domain ", domain, " doesn't match ", host)
		default:
			cookie.HostOnly, cookie.Domain = false, domain
		}
	}
	cookie.Path = path
	if !strings.HasPrefix(path, "/") {
		cookie.Path = cookiePath(u)
		if index := strings.LastIndex(cookie.Path, "/"); index > 0 {
			cookie.Path = cookie.Path[:index]
		} else {
			cookie.Path = "/"
		}
	}
	return cookie, nil
}

func storeCookie(cookies []storedCookie, u *url.URL, cookie storedCookie, now time.Time) ([]storedCookie, error) {
	cookies = slices.DeleteFunc(cookies, func(it storedCookie) bool { return it.expired(now) })
	secure := secureURL(u)
	if cookie.Secure && !secure {
		return cookies, E.New("the cookie is Secure, but ", u.Scheme, ": is not secure")
	}
	prefixed := strings.ToLower(cookie.Name)
	if cookie.Name == "" {
		prefixed = strings.ToLower(cookie.Value)
	}
	switch {
	case cookie.Name == "" && (strings.HasPrefix(prefixed, "__secure-") || strings.HasPrefix(prefixed, "__host-")):
		return cookies, E.New("a cookie without a name can't start with __Secure- or __Host-")
	case strings.HasPrefix(prefixed, "__secure-") && !cookie.Secure:
		return cookies, E.New("a __Secure- cookie needs Secure")
	case strings.HasPrefix(prefixed, "__host-") && (!cookie.Secure || !cookie.HostOnly || cookie.Path != "/"):
		return cookies, E.New("a __Host- cookie needs Secure, Path=/ and no Domain")
	}
	if !cookie.Secure && !secure && slices.ContainsFunc(cookies, func(it storedCookie) bool {
		return it.Secure && it.Name == cookie.Name && (domainMatch(it.Domain, cookie.Domain) || domainMatch(cookie.Domain, it.Domain)) && pathMatch(cookie.Path, it.Path)
	}) {
		return cookies, E.New("a Secure cookie named ", cookie.Name, " exists, and ", u.Scheme, ": can't replace it")
	}
	index := slices.IndexFunc(cookies, func(it storedCookie) bool {
		return it.Name == cookie.Name && it.Domain == cookie.Domain && it.HostOnly == cookie.HostOnly && it.Path == cookie.Path
	})
	switch {
	case cookie.expired(now) && index >= 0:
		return slices.Delete(cookies, index, index+1), nil
	case cookie.expired(now):
		return cookies, nil
	case index >= 0:
		cookies[index] = cookie
		return cookies, nil
	default:
		return append(cookies, cookie), nil
	}
}

var cookieMonths = []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}

func parseCookieDate(text string) (time.Time, bool) {
	delimiter := func(character rune) bool {
		return character == 0x09 || character >= 0x20 && character <= 0x2f || character >= 0x3b && character <= 0x40 ||
			character >= 0x5b && character <= 0x60 || character >= 0x7b && character <= 0x7e
	}
	var clock [3]int
	day, month, year := -1, -1, -1
	foundClock := false
	for _, token := range strings.FieldsFunc(text, delimiter) {
		switch {
		case !foundClock && cookieClock(token, &clock):
			foundClock = true
		case day < 0 && cookieNumber(token, 1, 2, &day):
		case month < 0 && len(token) >= 3 && slices.Contains(cookieMonths, strings.ToLower(token[:3])):
			month = slices.Index(cookieMonths, strings.ToLower(token[:3])) + 1
		case year < 0 && cookieNumber(token, 2, 4, &year):
		}
	}
	switch {
	case year >= 70 && year <= 99:
		year += 1900
	case year >= 0 && year <= 69:
		year += 2000
	}
	if !foundClock || day < 1 || day > 31 || month < 1 || year < 1601 || clock[0] > 23 || clock[1] > 59 || clock[2] > 59 {
		return time.Time{}, false
	}
	date := time.Date(year, time.Month(month), day, clock[0], clock[1], clock[2], 0, time.UTC)
	return date, date.Day() == day
}

func cookieNumber(token string, least, most int, out *int) bool {
	digits := 0
	for digits < len(token) && token[digits] >= '0' && token[digits] <= '9' {
		digits++
	}
	if digits < least || digits > most {
		return false
	}
	*out, _ = strconv.Atoi(token[:digits])
	return true
}

func cookieClock(token string, clock *[3]int) bool {
	parts := strings.SplitN(token, ":", 3)
	if len(parts) != 3 {
		return false
	}
	for index, part := range parts {
		if index < 2 && (len(part) < 1 || len(part) > 2) {
			return false
		}
		if !cookieNumber(part, 1, 2, &clock[index]) {
			return false
		}
	}
	return true
}

func domainMatch(host, domain string) bool {
	return host == domain || strings.HasSuffix(host, "."+domain) && net.ParseIP(host) == nil
}

func pathMatch(path, cookiePath string) bool {
	return path == cookiePath || strings.HasPrefix(path, cookiePath) && (strings.HasSuffix(cookiePath, "/") || path[len(cookiePath)] == '/')
}

func cookieURL(text string) (*url.URL, error) {
	u, err := url.Parse(text)
	if err != nil {
		return nil, E.Cause1(ErrInvalid, err)
	}
	switch u.Scheme {
	case "http", "https", "ws", "wss":
	default:
		return nil, E.Cause1(ErrInvalid, E.New("cookies belong to http:, https:, ws: and wss: URLs, not ", u.Scheme, ":"))
	}
	return u, nil
}

func cookiesGet(_ context.Context, call *operation) (any, error) {
	var reference jarReference
	var target string
	if err := call.decode(1, &reference, &target); err != nil {
		return nil, err
	}
	var u *url.URL
	if target != "" {
		var err error
		if u, err = cookieURL(target); err != nil {
			return nil, err
		}
	}
	jar, err := call.engine.cookieJar(reference)
	if err != nil {
		return nil, err
	}
	cookies, err := jar.matching(u)
	if cookies == nil {
		cookies = []storedCookie{}
	}
	return cookies, err
}

func cookiesSet(_ context.Context, call *operation) (any, error) {
	var reference jarReference
	var target, line string
	if err := call.decode(3, &reference, &target, &line); err != nil {
		return nil, err
	}
	u, err := cookieURL(target)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	cookie, err := parseSetCookie(line, u, now)
	if err != nil {
		return nil, E.Cause1(ErrInvalid, err)
	}
	jar, err := call.engine.cookieJar(reference)
	if err != nil {
		return nil, err
	}
	var rejected error
	err = jar.update(func(cookies []storedCookie) []storedCookie {
		stored, err := storeCookie(cookies, u, cookie, now)
		if err != nil {
			rejected = E.Cause1(ErrInvalid, err)
		}
		return stored
	})
	return nil, E.Errors(rejected, err)
}

func cookiesClear(_ context.Context, call *operation) (any, error) {
	var reference jarReference
	if err := call.decode(1, &reference); err != nil {
		return nil, err
	}
	jar, err := call.engine.cookieJar(reference)
	if err != nil {
		return nil, err
	}
	return nil, jar.clear()
}
