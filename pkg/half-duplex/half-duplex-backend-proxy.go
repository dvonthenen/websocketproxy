// Copyright 2022 The dvonthenen WebSocketProxy Authors. All Rights Reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.
// SPDX-License-Identifier: Apache-2.0

package halfduplexproxy

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dvonthenen/websocket"
	klog "k8s.io/klog/v2"

	common "github.com/dvonthenen/websocketproxy/pkg/common"
)

// NewProxy returns a new Websocket reverse proxy that rewrites the
// URL's to the scheme, host and base path provider in target.
func NewProxy(options common.ProxyOptions) *HalfDuplexWebsocketProxy {
	backend := func(r *http.Request) *url.URL {
		// Shallow copy
		u := options.Url
		u.Fragment = r.URL.Fragment
		u.Path = r.URL.Path
		u.RawQuery = r.URL.RawQuery
		return u
	}
	return &HalfDuplexWebsocketProxy{
		&common.WebsocketProxy{
			UniqueId: options.UniqueID,
			Director: options.Director,
			Viewer:   options.Viewer,
			Manager:  options.Manager,
			Upgrader: options.Upgrader,
			Dialer:   options.Dialer,
			Backend:  backend,
			Options:  options,
		},
		nil,
		nil,
	}
}

// ServeHTTP implements the http.Handler that proxies WebSocket connections.
func (w *HalfDuplexWebsocketProxy) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	if w.Backend == nil {
		klog.Errorf("websocketproxy: backend function is not defined\n")
		http.Error(rw, "internal server error (code: 1)", http.StatusInternalServerError)
		return
	}

	backendURL := w.Backend(req)
	if backendURL == nil {
		klog.Errorf("websocketproxy: backend URL is nil\n")
		http.Error(rw, "internal server error (code: 2)", http.StatusInternalServerError)
		return
	}

	// using a custom dialer?
	dialer := w.Dialer
	if w.Dialer == nil {
		dialer = common.DefaultDialer
	}

	// Pass headers from the incoming request to the dialer to forward them to
	// the final destinations.
	var requestHeader http.Header

	// enable more of a passthrough proxy
	if w.Options.NaturalTunnel {
		requestHeader = req.Header.Clone()

		/*
			Please see: https://github.com/koding/websocketproxy/pull/44/
		*/
		// dvonthenen/websocket automatically adds these headers back when Dial() is called, but it never
		// uses Set(), rather it sets these headers using normal assignment might can so lead to
		// duplicate headers. Hence, we can remove them. (If this problem gets fixed in dvonthenen/websocket,
		// these 5 lines become redundant, but will not break the current implementation)
		requestHeader.Del("Connection")
		requestHeader.Del("Sec-Websocket-Extensions")
		requestHeader.Del("Sec-Websocket-Key")
		requestHeader.Del("Sec-Websocket-Version")
		requestHeader.Del("Upgrade")

		// Remove all hop-by-hop headers
		requestHeader.Del("Keep-Alive")
		requestHeader.Del("Transfer-Encoding")
		requestHeader.Del("TE")
		requestHeader.Del("Trailer")
		requestHeader.Del("Proxy-Authorization")
		requestHeader.Del("Proxy-Authenticate")

	} else { // default library behavior
		requestHeader = http.Header{}

		if origin := req.Header.Get("User-Agent"); origin != "" {
			requestHeader.Add("User-Agent", origin)
		}
		if origin := req.Header.Get("Origin"); origin != "" {
			requestHeader.Add("Origin", origin)
		}
		for _, prot := range req.Header[http.CanonicalHeaderKey("Sec-WebSocket-Protocol")] {
			requestHeader.Add("Sec-WebSocket-Protocol", prot)
		}
		for _, cookie := range req.Header[http.CanonicalHeaderKey("Cookie")] {
			requestHeader.Add("Cookie", cookie)
		}
		if req.Host != "" {
			requestHeader.Set("Host", req.Host)
		}
	}

	// Pass X-Forwarded-For headers too, code below is a part of
	// httputil.ReverseProxy. See http://en.wikipedia.org/wiki/X-Forwarded-For
	// for more information use RFC7239 http://tools.ietf.org/html/rfc7239
	if clientIP, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
		// If we aren't the first proxy retain prior
		// X-Forwarded-For information as a comma+space
		// separated list and fold multiple headers into one.
		if prior, ok := req.Header["X-Forwarded-For"]; ok {
			clientIP = strings.Join(prior, ", ") + ", " + clientIP
		}
		requestHeader.Set("X-Forwarded-For", clientIP)
	}

	// Set the originating protocol of the incoming HTTP request. The SSL might
	// be terminated on our site and because we doing proxy adding this would
	// be helpful for applications on the backend.
	requestHeader.Set("X-Forwarded-Proto", "http")
	if req.TLS != nil {
		requestHeader.Set("X-Forwarded-Proto", "https")
	}

	// Enable the director to copy any additional headers it desires for
	// forwarding to the remote server.
	if w.Director != nil {
		w.Director.AdjustHeaders(req, requestHeader)
	}

	// Connect to the backend URL, also pass the headers we get from the request
	// together with the Forwarded headers we prepared above.
	// TODO: support multiplexing on the same backend connection instead of
	// opening a new TCP connection time for each request. This should be
	// optional:
	// http://tools.ietf.org/html/draft-ietf-hybi-websocket-multiplexing-01
	connBackend, resp, err := dialer.Dial(backendURL.String(), requestHeader)
	if err != nil {
		klog.Errorf("websocketproxy: couldn't dial to remote backend url %s\n", err)
		if resp != nil {
			// If the WebSocket handshake fails, ErrBadHandshake is returned
			// along with a non-nil *http.Response so that callers can handle
			// redirects, authentication, etcetera.
			if err := copyResponse(rw, resp); err != nil {
				klog.Errorf("websocketproxy: couldn't write response after failed remote backend handshake: %s\n", err)
			}
		} else {
			http.Error(rw, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		}
		return
	}

	// using a custom upgrader?
	upgrader := w.Upgrader
	if w.Upgrader == nil {
		upgrader = common.DefaultUpgrader
	}

	// Only pass those headers to the upgrader.
	var upgradeHeader http.Header

	// enable more of a passthrough proxy
	if w.Options.NaturalTunnel {
		upgradeHeader := req.Header.Clone()

		/*
			Please see: https://github.com/koding/websocketproxy/pull/44/
		*/
		// dvonthenen/websocket automatically adds these headers back when Dial() is called, but it never
		// uses Set(), rather it sets these headers using normal assignment might can so lead to
		// duplicate headers. Hence, we can remove them. (If this problem gets fixed in dvonthenen/websocket,
		// these 5 lines become redundant, but will not break the current implementation)
		upgradeHeader.Del("Connection")
		upgradeHeader.Del("Sec-Websocket-Extensions")
		upgradeHeader.Del("Sec-Websocket-Key")
		upgradeHeader.Del("Sec-Websocket-Version")
		upgradeHeader.Del("Upgrade")

		// Remove all hop-by-hop headers
		upgradeHeader.Del("Keep-Alive")
		upgradeHeader.Del("Transfer-Encoding")
		upgradeHeader.Del("TE")
		upgradeHeader.Del("Trailer")
		upgradeHeader.Del("Proxy-Authorization")
		upgradeHeader.Del("Proxy-Authenticate")
	} else { // default library behavior
		upgradeHeader = http.Header{}

		if hdr := resp.Header.Get("Sec-Websocket-Protocol"); hdr != "" {
			upgradeHeader.Set("Sec-Websocket-Protocol", hdr)
		}
		if hdr := resp.Header.Get("Set-Cookie"); hdr != "" {
			upgradeHeader.Set("Set-Cookie", hdr)
		}
		/*
			Please see: https://github.com/koding/websocketproxy/pull/40/
			when using more than one wss proxy, need add Sec-Websocket-Accept header
		*/
		if hdr := resp.Header.Get("Sec-Websocket-Accept"); hdr != "" {
			upgradeHeader.Set("Sec-Websocket-Accept", hdr)
		}
	}

	// Now upgrade the existing incoming request to a WebSocket connection.
	// Also pass the header that we gathered from the Dial handshake.
	connPub, err := upgrader.Upgrade(rw, req, upgradeHeader)
	if err != nil {
		klog.Errorf("websocketproxy: couldn't upgrade %s\n", err)
		return
	}

	// save the connections
	w.ToBackend = connBackend
	w.ToClient = connPub

	// worker threads
	errClient := make(chan error, 1)
	errBackend := make(chan error, 1)
	replicateWebsocketProxyToServer := func(dst, src *websocket.Conn, errc chan error, stopChan chan struct{}) {
		for {
			/*
				Please see: https://github.com/koding/websocketproxy/pull/36
				Useful when implementing authenticated proxy.
			*/
			// do until stopChan gets any message
			doExit := false
			select {
			default:
				msgType, msg, err := src.ReadMessage()
				if err != nil {
					m := websocket.FormatCloseMessage(websocket.CloseNormalClosure, fmt.Sprintf("%v", err))
					if e, ok := err.(*websocket.CloseError); ok {
						if e.Code != websocket.CloseNoStatusReceived {
							m = websocket.FormatCloseMessage(e.Code, e.Text)
						}
					}
					errc <- err
					dst.WriteMessage(websocket.CloseMessage, m)
					break
				}

				err = dst.WriteMessage(msgType, msg)
				if err != nil {
					errc <- err
					doExit = true
				}
			case <-stopChan:
				dst.WriteMessage(websocket.CloseMessage, []byte("Closed by proxy"))
				return
			}
			if doExit {
				break
			}
		}
	}

	replicateWebsocketClientToProxy := func(dst, src *websocket.Conn, errc chan error, stopChan chan struct{}) {
		for {
			/*
				Please see: https://github.com/koding/websocketproxy/pull/36
				Useful when implementing authenticated proxy.
			*/
			// do until stopChan gets any message
			doExit := false
			select {
			default:
				msgType, msg, err := src.ReadMessage()
				if err != nil {
					m := websocket.FormatCloseMessage(websocket.CloseNormalClosure, fmt.Sprintf("%v", err))
					if e, ok := err.(*websocket.CloseError); ok {
						if e.Code != websocket.CloseNoStatusReceived {
							m = websocket.FormatCloseMessage(e.Code, e.Text)
						}
					}
					errc <- err
					dst.WriteMessage(websocket.CloseMessage, m)
					break
				}

				// we only care about the messages and not the raw data
				if msgType == 1 && w.Viewer != nil {
					w.Viewer.HandleMessage(msg)
				}
			case <-stopChan:
				dst.WriteMessage(websocket.CloseMessage, []byte("Closed by proxy"))
				return
			}
			if doExit {
				break
			}
		}
	}

	/*
		Please see: https://github.com/koding/websocketproxy/pull/43
		Send a Ping message to the backend connection whenever a Ping is received.
	*/
	connPub.SetPingHandler(func(appData string) error {
		err := connBackend.WriteControl(websocket.PingMessage, []byte(appData), time.Now().Add(time.Second))
		if err != nil {
			return err
		}

		// default behavior from https://github.com/gorilla/websocket/blob/v1.5.0/conn.go#L1161-L1167
		err = connPub.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(time.Second))
		if err == websocket.ErrCloseSent {
			return nil
		} else if e, ok := err.(net.Error); ok && e.Temporary() {
			return nil
		}
		return err
	})

	w.WebsocketProxy.Connected = true

	go replicateWebsocketClientToProxy(connPub, connBackend, errClient, w.StopClientChan)
	go replicateWebsocketProxyToServer(connBackend, connPub, errBackend, w.StopBackendChan)

	var message string
	select {
	case err = <-errClient:
		message = "websocketproxy: Error when copying from backend to client: %v"
	case err = <-errBackend:
		message = "websocketproxy: Error when copying from client to backend: %v"

	}
	if e, ok := err.(*websocket.CloseError); !ok || e.Code == websocket.CloseAbnormalClosure {
		klog.Errorf("message: %s, err: %v\n", message, err)
	}

	w.WebsocketProxy.Connected = false

	if w.Manager != nil && len(w.WebsocketProxy.UniqueId) > 0 {
		w.Manager.RemoveConnection(w.UniqueId)
	}
}

// SendMessageByType To Client/Browser
func (w *HalfDuplexWebsocketProxy) SendMessage(data []byte) error {
	return w.SendMessageByType(common.MessageTypeControl, data)
}

// SendMessageByType To Client/Browser
func (w *HalfDuplexWebsocketProxy) SendMessageByType(msgType int, data []byte) error {
	if w.ToClient == nil {
		return common.ErrConnectionNotEstablished
	}

	err := w.ToClient.WriteMessage(msgType, data)
	if err != nil {
		return err
	}

	return nil
}

// IsConnected
func (w *HalfDuplexWebsocketProxy) IsConnected() bool {
	return w.WebsocketProxy.Connected
}

// Stop websocket proxy on demand
func (w *HalfDuplexWebsocketProxy) CloseProxy() {
	if w.ToBackend != nil {
		w.ToBackend.Close()
		w.ToBackend = nil
	}
	if w.ToClient != nil {
		w.ToClient.Close()
		w.ToClient = nil
	}

	close(w.StopBackendChan)
	close(w.StopClientChan)
}

func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func copyResponse(rw http.ResponseWriter, resp *http.Response) error {
	copyHeader(rw.Header(), resp.Header)
	rw.WriteHeader(resp.StatusCode)
	defer resp.Body.Close()

	_, err := io.Copy(rw, resp.Body)
	return err
}
