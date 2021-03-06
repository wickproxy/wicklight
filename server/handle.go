package server

import (
	"errors"
	"net"
	"net/http"
	"wicklight/config"
	"wicklight/logger"
	"wicklight/quota"
	"wicklight/transport"
)

var (
	errGateway = errors.New("gateway error")
)

type server struct{}

type request struct {
	user string
	host string
	port string

	authenticated bool
	allowed       bool
	notexceeded   bool

	err error
}

func (s *server) ServeHTTP(w http.ResponseWriter, hr *http.Request) {
	req := request{}
	req.host, req.port = checkHost(hr)
	req.user, req.authenticated = checkUser(hr)
	req.allowed = checkACL(req)
	req.notexceeded = quota.CheckQuota(req.user)

	if req.authenticated && req.allowed && req.notexceeded {
		logger.Debugf("[proxy] %v(%v) to %v %v:%v authenticated: %v, allowd: %v, not restricted: %v", req.user, quota.PrintQuota(req.user), hr.Method, req.host, req.port, req.authenticated, req.allowed, req.notexceeded)
	} else {
		logger.Warnf("[proxy] %v(%v) to %v %v:%v authenticated: %v, allowd: %v, not restricted: %v", req.user, quota.PrintQuota(req.user), hr.Method, req.host, req.port, req.authenticated, req.allowed, req.notexceeded)
	}

	if req.host == config.Conf.Fallback.Host {
		handlePanel(w, hr, req)
		return
	}

	if !req.authenticated || !req.allowed || !req.notexceeded {
		if req.authenticated || isHostInWhiteList(req.host) {
			handlePanel(w, hr, req)
			return
		} else if config.Conf.Fallback.Target != "" {
			handleReverseProxy(config.Conf.Fallback.Target, w, hr, false)
			return
		} else {
			return
		}
	}

	if hr.Method == http.MethodConnect {
		handleProxyConnect(w, hr, &req)
	} else {
		handleProxyRaw(w, hr, &req)
	}
}

func handleProxyConnect(w http.ResponseWriter, r *http.Request, req *request) {
	wFlusher, ok := w.(http.Flusher)
	if !ok {
		req.err = errors.New("Do not support flusher")
		logger.Debug("[proxy]", req.err)
		handlePanel(w, r, *req)
		return
	}

	w.WriteHeader(200)
	wFlusher.Flush()

	hostPort := net.JoinHostPort(req.host, req.port)

	outbound, err := net.Dial("tcp", hostPort)
	if err != nil {
		req.err = errGateway
		logger.Debug("[outbound]", err)
		handlePanel(w, r, *req)
		return
	}
	defer outbound.Close()

	switch r.ProtoMajor {
	case 1:
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			req.err = errors.New("Do not support hijacker")
			logger.Debug("[proxy]", req.err)
			handlePanel(w, r, *req)
			return
		}

		client, bufReader, err := hijacker.Hijack()
		if err != nil {
			req.err = err
			handlePanel(w, r, *req)
			return
		}

		if n := bufReader.Reader.Buffered(); n > 0 {
			rbuf, err := bufReader.Reader.Peek(n)
			if err != nil {
				req.err = err
				handlePanel(w, r, *req)
				return
			}
			outbound.Write(rbuf)
		}
		usage := transport.Relay(outbound, client, client)
		quota.UpdateQuota(req.user, usage)
	default:
		defer r.Body.Close()
		usage := transport.Relay(outbound, r.Body, w)
		quota.UpdateQuota(req.user, usage)
	}
}

func handleProxyRaw(w http.ResponseWriter, r *http.Request, req *request) {
	if r.URL.Scheme == "" {
		r.URL.Scheme = "http"
	}
	if r.URL.Host == "" {
		r.URL.Host = net.JoinHostPort(req.host, req.port)
	}
	handleReverseProxy(r.URL.String(), w, r, true)
}
