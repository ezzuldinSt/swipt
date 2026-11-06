package automation

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	E "github.com/sagernet/sing/common/exceptions"

	"github.com/andybalholm/brotli"
)

const (
	userAgent     = "Swipt"
	redirectLimit = 20
)

var errNetwork = E.New("network error")

var nullBodyStatuses = []int{http.StatusSwitchingProtocols, http.StatusEarlyHints, http.StatusNoContent, http.StatusResetContent, http.StatusNotModified}

type clientOptions struct {
	Proxy              string        `json:"proxy"`
	CA                 []string      `json:"ca"`
	RejectUnauthorized *bool         `json:"rejectUnauthorized"`
	Cookies            *jarReference `json:"cookies"`
}

type transportKey struct {
	proxy    string
	ca       string
	insecure bool
	tunnel   bool
}

func (h *Host) transport(options clientOptions, tunnel bool) (*http.Transport, error) {
	key := transportKey{
		proxy: options.Proxy, ca: strings.Join(options.CA, "\n"), insecure: options.RejectUnauthorized != nil && !*options.RejectUnauthorized,
		tunnel: tunnel && (strings.HasPrefix(options.Proxy, "http:") || strings.HasPrefix(options.Proxy, "https:")),
	}
	if cached, loaded := h.transports.Load(key); loaded {
		return cached.(*http.Transport), nil
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true
	transport.ProxyConnectHeader = http.Header{"User-Agent": {userAgent}}
	if len(options.CA) > 0 || key.insecure {
		config := &tls.Config{InsecureSkipVerify: key.insecure}
		if len(options.CA) > 0 {
			config.RootCAs = x509.NewCertPool()
			for _, certificate := range options.CA {
				if !config.RootCAs.AppendCertsFromPEM([]byte(certificate)) {
					return nil, E.Cause1(ErrInvalid, E.New("the CA certificate has no PEM certificates"))
				}
			}
		}
		transport.TLSClientConfig = config
	}
	if options.Proxy != "" {
		proxy, err := url.Parse(options.Proxy)
		if err != nil {
			return nil, E.Cause1(ErrInvalid, E.Cause(err, "parse the proxy"))
		}
		switch {
		case key.tunnel:
			transport.Proxy = nil
			transport.DialContext = func(ctx context.Context, _, address string) (net.Conn, error) {
				return dialTunnel(ctx, proxy, address, transport.TLSClientConfig)
			}
		case proxy.Scheme == "https":
			plain := *proxy
			plain.Scheme = "http"
			transport.Proxy = http.ProxyURL(&plain)
			transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialProxy(ctx, proxy, transport.TLSClientConfig)
			}
		default:
			transport.Proxy = http.ProxyURL(proxy)
		}
	}
	cached, _ := h.transports.LoadOrStore(key, transport)
	return cached.(*http.Transport), nil
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c bufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

func dialProxy(ctx context.Context, proxy *url.URL, config *tls.Config) (net.Conn, error) {
	port := proxy.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[proxy.Scheme]
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(proxy.Hostname(), port))
	if err != nil || proxy.Scheme != "https" {
		return conn, err
	}
	proxyConfig := &tls.Config{}
	if config != nil {
		proxyConfig = config.Clone()
	}
	proxyConfig.ServerName = proxy.Hostname()
	proxyConfig.NextProtos = []string{"http/1.1"}
	secured := tls.Client(conn, proxyConfig)
	if err = secured.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, E.Cause(err, "connect to the proxy")
	}
	return secured, nil
}

func dialTunnel(ctx context.Context, proxy *url.URL, address string, config *tls.Config) (net.Conn, error) {
	conn, err := dialProxy(ctx, proxy, config)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	request := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: address}, Host: address, Header: http.Header{"User-Agent": {userAgent}}}
	if proxy.User != nil {
		password, _ := proxy.User.Password()
		request.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(proxy.User.Username()+":"+password)))
	}
	if err = request.Write(conn); err != nil {
		_ = conn.Close()
		return nil, E.Cause(err, "connect to the proxy")
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		_ = conn.Close()
		return nil, E.Cause(err, "connect to the proxy")
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, E.New("the proxy answered the tunnel with ", response.Status)
	}
	return bufferedConn{Conn: conn, reader: reader}, nil
}

func (e *engine) httpClient(options clientOptions, tunnel bool) (*http.Client, error) {
	transport, err := e.host.transport(options, tunnel)
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}, nil
}

func byteString(text string) string {
	if isASCII(text) {
		return text
	}
	out := make([]byte, 0, len(text))
	for _, character := range text {
		out = append(out, byte(character))
	}
	return string(out)
}

func latin1(raw string) string {
	if isASCII(raw) {
		return raw
	}
	var out strings.Builder
	for index := range len(raw) {
		out.WriteRune(rune(raw[index]))
	}
	return out.String()
}

func isASCII(text string) bool {
	for index := range len(text) {
		if text[index] >= 0x80 {
			return false
		}
	}
	return true
}

func escapeNonASCII(text string) string {
	if isASCII(text) {
		return text
	}
	var out strings.Builder
	for index := range len(text) {
		if character := text[index]; character >= 0x80 {
			out.WriteByte('%')
			out.WriteByte("0123456789ABCDEF"[character>>4])
			out.WriteByte("0123456789ABCDEF"[character&0xf])
		} else {
			out.WriteByte(character)
		}
	}
	return out.String()
}

func networkError(err error, message ...any) error {
	if urlError, isURL := errors.AsType[*url.Error](err); isURL {
		err = urlError.Err
	}
	return E.Cause1(errNetwork, E.Cause(err, message...))
}

type networkReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r networkReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err == nil || errors.Is(err, io.EOF) {
		return n, err
	}
	if _, aborted := errors.AsType[*Failure](context.Cause(r.ctx)); aborted {
		return n, context.Cause(r.ctx)
	}
	if !E.IsMulti(err, errReleased, errEnded) {
		err = networkError(err, "read the response body")
	}
	return n, err
}

type uploadBody struct {
	*resource
}

func (b uploadBody) Read(p []byte) (int, error) {
	return b.reader.Read(p)
}

func (b uploadBody) Close() error {
	return b.close(io.ErrClosedPipe)
}

func streamCreate(_ context.Context, call *operation) (any, error) {
	reader, writer := pipeResources()
	return []string{call.engine.addResource(reader), call.engine.addResource(writer)}, nil
}

type fetchRequest struct {
	URL         string      `json:"url"`
	Method      string      `json:"method"`
	Headers     [][2]string `json:"headers"`
	Body        string      `json:"body"`
	Stream      string      `json:"stream"`
	Redirect    string      `json:"redirect"`
	Credentials string      `json:"credentials"`
}

func fetch(ctx context.Context, call *operation) (any, error) {
	var request fetchRequest
	var options clientOptions
	if err := call.decode(1, &request, &options); err != nil {
		return nil, err
	}
	e := call.engine
	var data []byte
	var upload *resource
	switch {
	case request.Body != "":
		var err error
		if data, err = e.takeBlob(request.Body); err != nil {
			return nil, err
		}
	case request.Stream != "":
		var err error
		if upload, err = e.takeResource(request.Stream); err != nil {
			return nil, err
		}
		if upload.reader == nil {
			_ = upload.close(errReleased)
			return nil, E.Cause1(ErrInvalid, E.New("the request body is not readable"))
		}
	}
	dropUpload := func(err error) {
		if upload != nil {
			_ = upload.close(err)
			upload = nil
		}
	}
	client, err := e.httpClient(options, false)
	if err != nil {
		dropUpload(err)
		return nil, err
	}
	var jar *cookieJar
	if options.Cookies != nil && request.Credentials != "omit" {
		if jar, err = e.cookieJar(*options.Cookies); err != nil {
			dropUpload(err)
			return nil, err
		}
	}
	requestCtx, cancel := context.WithCancelCause(e.ctx)
	newRequest := func(method, target string, header http.Header, host string) (*http.Request, error) {
		var body io.Reader
		if data != nil {
			body = bytes.NewReader(data)
		}
		outgoing, err := http.NewRequestWithContext(requestCtx, method, target, body)
		if err != nil {
			return nil, E.Cause1(ErrInvalid, err)
		}
		if upload != nil {
			outgoing.Body = uploadBody{upload}
			outgoing.ContentLength = -1
		}
		outgoing.Header = header.Clone()
		outgoing.Host = host
		return outgoing, nil
	}
	header := make(http.Header)
	host := ""
	for _, field := range request.Headers {
		name, value := byteString(field[0]), byteString(field[1])
		if strings.EqualFold(name, "host") {
			host = value
		} else {
			header.Add(name, value)
		}
	}
	for name, value := range map[string]string{"User-Agent": userAgent, "Accept": "*/*", "Accept-Language": "*", "Accept-Encoding": "gzip, deflate, br"} {
		if _, set := header[name]; !set {
			header.Set(name, value)
		}
	}
	outgoing, err := newRequest(request.Method, request.URL, header, host)
	if err != nil {
		cancel(nil)
		dropUpload(err)
		return nil, err
	}
	uploaded := upload
	stop := context.AfterFunc(ctx, func() {
		cancel(context.Cause(ctx))
		if uploaded != nil {
			_ = uploaded.close(context.Cause(ctx))
		}
	})
	response, redirected, err := send(client, jar, outgoing, request.Redirect, header, newRequest, dropUpload)
	if !stop() {
		if err == nil {
			_ = response.Body.Close()
		}
		cancel(nil)
		return nil, ctx.Err()
	}
	if err != nil {
		cancel(nil)
		dropUpload(err)
		return nil, networkError(err, "fetch ", request.URL)
	}
	final := *response.Request.URL
	final.Fragment, final.RawFragment = "", ""
	statusText := strings.TrimSpace(strings.TrimPrefix(response.Status, strconv.Itoa(response.StatusCode)))
	if response.ProtoMajor >= 2 {
		statusText = ""
	}
	var headers [][2]string
	for name, values := range response.Header {
		for _, value := range values {
			headers = append(headers, [2]string{latin1(name), latin1(value)})
		}
	}
	out := map[string]any{
		"status": response.StatusCode, "statusText": latin1(statusText),
		"url": escapeNonASCII(final.String()), "redirected": redirected, "headers": headers, "body": "",
	}
	if response.Request.Method == http.MethodHead || slices.Contains(nullBodyStatuses, response.StatusCode) {
		_ = response.Body.Close()
		cancel(nil)
		return out, nil
	}
	out["body"] = e.addResource(&resource{
		reader:    networkReader{ctx: requestCtx, reader: decodeContent(response.Header.Values("Content-Encoding"), response.Body)},
		interrupt: cancel,
		finish: func(abort error) error {
			cancel(abort)
			return response.Body.Close()
		},
	})
	return out, nil
}

type requestFactory func(method, target string, header http.Header, host string) (*http.Request, error)

func send(client *http.Client, jar *cookieJar, outgoing *http.Request, mode string, header http.Header, newRequest requestFactory, dropUpload func(error)) (*http.Response, bool, error) {
	redirected := false
	for redirects := 0; ; redirects++ {
		if jar != nil {
			cookies, err := jar.header(outgoing.URL)
			if err != nil {
				return nil, false, err
			}
			attachCookies(outgoing.Header, cookies)
		}
		response, err := client.Do(outgoing)
		if err != nil {
			return nil, false, err
		}
		if jar != nil {
			if err = jar.store(outgoing.URL, response.Header.Values("Set-Cookie")); err != nil {
				_ = response.Body.Close()
				return nil, false, err
			}
		}
		switch response.StatusCode {
		case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		default:
			return response, redirected, nil
		}
		locations, hasLocation := response.Header["Location"]
		if mode == "manual" || !hasLocation {
			return response, redirected, nil
		}
		_ = response.Body.Close()
		target, err := outgoing.URL.Parse(escapeNonASCII(locations[0]))
		switch {
		case err != nil:
			return nil, false, E.Cause(err, "parse the redirect location")
		case mode == "error":
			return nil, false, E.New("the response redirected, and the request's redirect is error")
		case redirects >= redirectLimit:
			return nil, false, E.New("the response redirected more than ", redirectLimit, " times")
		case target.Scheme != "http" && target.Scheme != "https":
			return nil, false, E.New("the response redirected to ", target.Scheme, ":")
		}
		method := outgoing.Method
		status := response.StatusCode
		if (status == http.StatusMovedPermanently || status == http.StatusFound) && method == http.MethodPost || status == http.StatusSeeOther && method != http.MethodGet && method != http.MethodHead {
			method = http.MethodGet
			dropUpload(io.ErrClosedPipe)
			for _, name := range []string{"Content-Encoding", "Content-Language", "Content-Location", "Content-Type"} {
				header.Del(name)
			}
		} else if outgoing.Body != nil && outgoing.GetBody == nil {
			return nil, false, E.New("the response redirected with ", status, ", which needs the request body again, but the body is a stream")
		}
		host := outgoing.Host
		if target.Scheme != outgoing.URL.Scheme || target.Host != outgoing.URL.Host {
			host = ""
			for _, name := range []string{"Authorization", "Cookie", "Proxy-Authorization"} {
				header.Del(name)
			}
		}
		if outgoing, err = newRequest(method, target.String(), header, host); err != nil {
			return nil, false, err
		}
		if method == http.MethodGet || method == http.MethodHead {
			outgoing.Body, outgoing.GetBody, outgoing.ContentLength = nil, nil, 0
		}
		redirected = true
	}
}

func decodeContent(headers []string, body io.Reader) io.Reader {
	var codings []string
	for _, header := range headers {
		for coding := range strings.SplitSeq(header, ",") {
			coding = strings.ToLower(strings.TrimSpace(coding))
			switch coding {
			case "", "identity":
			case "gzip", "x-gzip", "deflate", "br":
				codings = append(codings, coding)
			default:
				return body
			}
		}
	}
	reader := body
	for _, coding := range slices.Backward(codings) {
		reader = &lazyDecoder{source: reader, coding: coding}
	}
	return reader
}

type lazyDecoder struct {
	source  io.Reader
	coding  string
	decoder io.Reader
}

func (d *lazyDecoder) Read(p []byte) (int, error) {
	if d.decoder == nil {
		buffered := bufio.NewReader(d.source)
		switch d.coding {
		case "br":
			d.decoder = brotli.NewReader(buffered)
		case "deflate":
			header, err := buffered.Peek(2)
			if err != nil {
				return 0, err
			}
			if header[0]&0x0f == 8 && (uint16(header[0])<<8|uint16(header[1]))%31 == 0 {
				decoder, err := zlib.NewReader(buffered)
				if err != nil {
					return 0, err
				}
				d.decoder = decoder
			} else {
				d.decoder = flate.NewReader(buffered)
			}
		default:
			decoder, err := gzip.NewReader(buffered)
			if err != nil {
				return 0, err
			}
			d.decoder = decoder
		}
	}
	return d.decoder.Read(p)
}
