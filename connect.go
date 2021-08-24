package klient

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"

	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

type connectDialer struct {
	ProxyURL      url.URL
	DefaultHeader http.Header

	Dialer  net.Dialer
	DialTLS func(network string, address string) (net.Conn, string, error)

	EnableH2ConnReuse  bool
	cacheH2Mu          sync.Mutex
	cachedH2ClientConn *http2.ClientConn
	cachedH2RawConn    net.Conn
}

func newConnectDialer(proxyURLStr string) (proxy.ContextDialer, error) {
	proxyURL, err := url.Parse(proxyURLStr)

	if err != nil {
		return nil, err
	}

	if proxyURL.Host == "" {
		return nil, fmt.Errorf("Invalid url \"%v\"\nFormat: https://username:password@hostname.com:443/", proxyURLStr)
	}

	switch proxyURL.Scheme {
	case "http":
		if proxyURL.Port() == "" {
			proxyURL.Host = net.JoinHostPort(proxyURL.Host, "80")
		}
	case "https":
		if proxyURL.Port() == "" {
			proxyURL.Host = net.JoinHostPort(proxyURL.Host, "443")
		}
	case "":
		return nil, errors.New("Proxy protocol missing")
	default:
		return nil, fmt.Errorf("Proxy protocol \"%v\" is invalid", proxyURL.Scheme)
	}

	client := &connectDialer{
		ProxyURL:          *proxyURL,
		DefaultHeader:     make(http.Header),
		EnableH2ConnReuse: true,
	}

	if proxyURL.User != nil {
		if proxyURL.User.Username() != "" {
			username := proxyURL.User.Username()
			password, _ := proxyURL.User.Password()

			auth := username + ":" + password
			basicAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(auth))
			client.DefaultHeader.Add("proxy-authorization", basicAuth)
		}
	}

	return client, nil
}

func (c *connectDialer) Dial(network, address string) (net.Conn, error) {
	return c.DialContext(context.Background(), network, address)
}

type ContextKeyHeader struct{}

func (c *connectDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	req := (&http.Request{
		Method: "CONNECT",
		URL:    &url.URL{Host: address},
		Header: make(http.Header),
		Host:   address,
	}).WithContext(ctx)

	for k, v := range c.DefaultHeader {
		req.Header[k] = v
	}

	if ctxHeader, ctxHasHeader := ctx.Value(&ContextKeyHeader{}).(http.Header); ctxHasHeader {
		for k, v := range ctxHeader {
			req.Header[k] = v
		}
	}

	connectHTTP2 := func(rawConn net.Conn, h2clientConn *http2.ClientConn) (net.Conn, error) {
		req.Proto = "HTTP/2.0"
		req.ProtoMajor = 2
		req.ProtoMinor = 0

		pr, pw := io.Pipe()
		req.Body = pr

		resp, err := h2clientConn.RoundTrip(req)

		if err != nil {
			_ = rawConn.Close()
			return nil, err
		}

		if resp.StatusCode != http.StatusOK {
			_ = rawConn.Close()
			return nil, errors.New("Proxy responded with: " + resp.Status)
		}

		return newHTTP2Conn(rawConn, pw, resp.Body), nil
	}

	connectHTTP1 := func(rawConn net.Conn) (net.Conn, error) {
		req.Proto = "HTTP/1.1"
		req.ProtoMajor = 1
		req.ProtoMinor = 1

		err := req.Write(rawConn)

		if err != nil {
			_ = rawConn.Close()
			return nil, err
		}

		resp, err := http.ReadResponse(bufio.NewReader(rawConn), req)

		if err != nil {
			_ = rawConn.Close()
			return nil, err
		}

		if resp.StatusCode != http.StatusOK {
			_ = rawConn.Close()
			return nil, errors.New("Proxy responded with: " + resp.Status)
		}

		return rawConn, nil
	}

	if c.EnableH2ConnReuse {
		c.cacheH2Mu.Lock()
		unlocked := false

		if c.cachedH2ClientConn != nil && c.cachedH2RawConn != nil {
			if c.cachedH2ClientConn.CanTakeNewRequest() {
				rc := c.cachedH2RawConn
				cc := c.cachedH2ClientConn

				c.cacheH2Mu.Unlock()

				unlocked = true

				proxyConn, err := connectHTTP2(rc, cc)

				if err == nil {
					return proxyConn, err
				}
				// else: carry on and try again
			}
		}

		if !unlocked {
			c.cacheH2Mu.Unlock()
		}
	}

	var err error
	var rawConn net.Conn

	negotiatedProtocol := ""

	switch c.ProxyURL.Scheme {
	case "http":
		rawConn, err = c.Dialer.DialContext(ctx, network, c.ProxyURL.Host)

		if err != nil {
			return nil, err
		}
	case "https":
		if c.DialTLS != nil {
			rawConn, negotiatedProtocol, err = c.DialTLS(network, c.ProxyURL.Host)

			if err != nil {
				return nil, err
			}
		} else {
			tlsConf := tls.Config{
				NextProtos: []string{"h2", "http/1.1"},
				ServerName: c.ProxyURL.Hostname(),
			}

			tlsConn, err := tls.Dial(network, c.ProxyURL.Host, &tlsConf)

			if err != nil {
				return nil, err
			}

			err = tlsConn.Handshake()

			if err != nil {
				return nil, err
			}

			negotiatedProtocol = tlsConn.ConnectionState().NegotiatedProtocol

			rawConn = tlsConn
		}
	default:
		return nil, fmt.Errorf("Scheme \"%v\" is invalid", c.ProxyURL.Scheme)
	}

	switch negotiatedProtocol {
	case "":
		fallthrough
	case "http/1.1":
		return connectHTTP1(rawConn)
	case "h2":
		t := &http2.Transport{}

		h2clientConn, err := t.NewClientConn(rawConn)

		if err != nil {
			_ = rawConn.Close()
			return nil, err
		}

		proxyConn, err := connectHTTP2(rawConn, h2clientConn)

		if err != nil {
			_ = rawConn.Close()
			return nil, err
		}

		if c.EnableH2ConnReuse {
			c.cacheH2Mu.Lock()
			c.cachedH2ClientConn = h2clientConn
			c.cachedH2RawConn = rawConn
			c.cacheH2Mu.Unlock()
		}

		return proxyConn, err
	default:
		_ = rawConn.Close()

		return nil, errors.New("Negotiated an unsupported application layer protocol: \"" +
			negotiatedProtocol + "\"")
	}
}

func newHTTP2Conn(c net.Conn, pipedReqBody *io.PipeWriter, respBody io.ReadCloser) net.Conn {
	return &http2Conn{Conn: c, in: pipedReqBody, out: respBody}
}

type http2Conn struct {
	net.Conn
	in  *io.PipeWriter
	out io.ReadCloser
}

func (h *http2Conn) Read(p []byte) (n int, err error) {
	return h.out.Read(p)
}

func (h *http2Conn) Write(p []byte) (n int, err error) {
	return h.in.Write(p)
}

func (h *http2Conn) Close() error {
	var retErr error = nil

	if err := h.in.Close(); err != nil {
		retErr = err
	}

	if err := h.out.Close(); err != nil {
		retErr = err
	}

	return retErr
}

func (h *http2Conn) CloseConn() error {
	return h.Conn.Close()
}

func (h *http2Conn) CloseWrite() error {
	return h.in.Close()
}

func (h *http2Conn) CloseRead() error {
	return h.out.Close()
}
