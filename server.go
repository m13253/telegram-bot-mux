package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/handlers"
)

type Server struct {
	conf       *Config
	db         *Database
	c          *Client
	httpServer http.Server
	listener   net.Listener
}

func NewServer(conf *Config, db *Database, c *Client) (*Server, error) {
	s := &Server{
		conf: conf,
		db:   db,
		c:    c,
	}
	s.httpServer.Handler = handlers.CombinedLoggingHandler(os.Stdout, handlers.CompressHandler(s))
	var err error
	s.listener, err = net.Listen("tcp", conf.Downstream.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to start HTTP server: %v", err)
	}
	log.Println("HTTP server is listening on", s.listener.Addr())
	return s, nil
}

func (s *Server) Close() error {
	return s.httpServer.Close()
}

func (s *Server) Serve() error {
	err := s.httpServer.Serve(s.listener)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func (s *Server) matchApiUrl(r *http.Request) (string, int) {
	prefixSegCount := len(s.conf.Downstream.ApiPrefix)
	path := strings.SplitN(r.URL.EscapedPath(), "/", prefixSegCount+1)
	for i := range prefixSegCount {
		if i >= len(path) {
			return "", http.StatusNotFound
		} else if i == prefixSegCount-1 {
			seg, err := url.PathUnescape(path[i])
			if err != nil || !strings.HasPrefix(seg, s.conf.Downstream.ApiPrefix[i]) {
				return "", http.StatusNotFound
			}
			if seg != s.conf.Downstream.ApiPrefix[i]+s.conf.Downstream.AuthToken {
				return "", http.StatusUnauthorized
			}
		} else {
			seg, err := url.PathUnescape(path[i])
			if err != nil || seg != s.conf.Downstream.ApiPrefix[i] {
				return "", http.StatusNotFound
			}
		}
	}
	if len(path) != prefixSegCount+1 {
		return "", http.StatusNotFound
	}
	return path[prefixSegCount], http.StatusOK
}

func (s *Server) matchFileUrl(r *http.Request) (string, int) {
	prefixSegCount := len(s.conf.Downstream.FilePrefix)
	path := strings.SplitN(r.URL.EscapedPath(), "/", prefixSegCount+1)
	for i := range prefixSegCount {
		if i >= len(path) {
			return "", http.StatusNotFound
		} else if i == prefixSegCount-1 {
			seg, err := url.PathUnescape(path[i])
			if err != nil || !strings.HasPrefix(seg, s.conf.Downstream.FilePrefix[i]) {
				return "", http.StatusNotFound
			}
			if seg != s.conf.Downstream.FilePrefix[i]+s.conf.Downstream.AuthToken {
				return "", http.StatusUnauthorized
			}
		} else {
			seg, err := url.PathUnescape(path[i])
			if err != nil || seg != s.conf.Downstream.FilePrefix[i] {
				return "", http.StatusNotFound
			}
		}
	}
	if len(path) != prefixSegCount+1 {
		return "", http.StatusNotFound
	}
	return path[prefixSegCount], http.StatusOK
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method, code := s.matchApiUrl(r)
	if code != http.StatusNotFound {
		if code != http.StatusOK {
			s.reportError(w, code)
			return
		}
		if method == "getUpdates" {
			s.getUpdates(w, r)
			return
		}
		s.forwardAPI(w, r, method)
		return
	}
	fileID, code := s.matchFileUrl(r)
	if code != http.StatusNotFound {
		if code != http.StatusOK {
			s.reportError(w, code)
			return
		}
		s.forwardFile(w, r, fileID)
		return
	}
	s.reportError(w, code)
}

func (s *Server) getUpdates(w http.ResponseWriter, r *http.Request) {
	// It seems the official API server ignores errors
	_ = r.ParseMultipartForm(10 << 20)
	offset, _ := strconv.ParseInt(r.FormValue("offset"), 10, 64)
	limit, _ := strconv.ParseUint(r.FormValue("limit"), 10, 64)
	timeout, _ := strconv.ParseUint(r.FormValue("timeout"), 10, 64)

	if offset == 0 {
		offset = -1
	}
	if limit == 0 || limit > 100 {
		limit = 100
	}
	timer := time.After(time.Duration(timeout) * time.Second)

	for {
		update, cancel := s.db.SubscribeNextUpdate()
		updatesReceived := false
		for updateJSON, err := range s.db.GetUpdates(r.Context(), offset, limit) {
			if err != nil {
				cancel()
				s.internalServerErrorHandler(w, err)
				return
			}
			if !updatesReceived {
				h := w.Header()
				h.Set("Content-Type", "application/json")
				h.Set("X-Content-Type-Options", "nosniff")
				w.Write([]byte("{\"ok\":true,\"result\":["))
			} else {
				w.Write([]byte{','})
			}
			updatesReceived = true
			fmt.Fprint(w, updateJSON)
		}
		if updatesReceived {
			cancel()
			w.Write([]byte("]}"))
			return
		}

		select {
		case <-timer:
			cancel()
			h := w.Header()
			h.Set("Content-Type", "application/json")
			h.Set("X-Content-Type-Options", "nosniff")
			w.Write([]byte("{\"ok\":true,\"result\":[]}"))
			return
		case <-update:
		}
	}
}

func (s *Server) forwardAPI(w http.ResponseWriter, r *http.Request, method string) {
	err := s.c.ForwardAPI(r.Context(), w, r, method)
	if err != nil {
		log.Println("API forward error:", err)
		s.reportError(w, http.StatusBadGateway)
	}
}

func (s *Server) forwardFile(w http.ResponseWriter, r *http.Request, method string) {
	err := s.c.ForwardFile(r.Context(), w, r, method)
	if err != nil {
		log.Println("File forward error:", err)
		s.reportError(w, http.StatusBadGateway)
	}
}

func (s *Server) reportError(w http.ResponseWriter, code int) {
	h := w.Header()
	h.Del("Content-Length")
	h.Set("Content-Type", "application/json")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	fmt.Fprintf(w, "{\"ok\":false,\"error_code\":%d,\"description\":%s}", code, http.StatusText(code))
}

func (s *Server) internalServerErrorHandler(w http.ResponseWriter, err error) {
	log.Println("Error:", err)
	debug.PrintStack()
	s.reportError(w, http.StatusInternalServerError)
}
