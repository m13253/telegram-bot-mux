package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
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

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	funcName, code := s.matchPrefix(r, s.c.conf.Downstream.ApiPrefix)
	if code != http.StatusNotFound {
		if code != http.StatusOK {
			s.ReportError(w, code)
		} else if funcName == "getUpdates" {
			s.getUpdates(w, r)
		} else {
			s.forwardAPI(w, r, funcName)
		}
		return
	}
	fileID, code := s.matchPrefix(r, s.c.conf.Downstream.FilePrefix)
	if code != http.StatusNotFound {
		if code != http.StatusOK {
			s.ReportError(w, code)
		} else {
			s.forwardFileRequest(w, r, fileID)
		}
		return
	}
	s.ReportError(w, http.StatusNotFound)
}

func (s *Server) matchPrefix(r *http.Request, prefix []string) (string, int) {
	prefixSegCount := len(prefix)
	path := strings.SplitN(r.URL.EscapedPath(), "/", prefixSegCount+1)
	for i := range prefixSegCount {
		if i >= len(path) {
			return "", http.StatusNotFound
		} else if i == prefixSegCount-1 {
			seg, err := url.PathUnescape(path[i])
			if err != nil || !strings.HasPrefix(seg, prefix[i]) {
				return "", http.StatusNotFound
			}
			if seg != prefix[i]+s.conf.Downstream.AuthToken {
				return "", http.StatusUnauthorized
			}
		} else {
			seg, err := url.PathUnescape(path[i])
			if err != nil || seg != prefix[i] {
				return "", http.StatusNotFound
			}
		}
	}
	if len(path) != prefixSegCount+1 {
		return "", http.StatusNotFound
	}
	return path[prefixSegCount], http.StatusOK
}

func (s *Server) getUpdates(w http.ResponseWriter, r *http.Request) {
	params := struct {
		Offset  int64  `json:"offset"`
		Limit   uint64 `json:"limit"`
		Timeout uint64 `json:"timeout"`
	}{}

	// Fetch request parameters. Ignore errors, just like the official API server
	params.Offset, _ = strconv.ParseInt(r.FormValue("offset"), 10, 64)
	params.Limit, _ = strconv.ParseUint(r.FormValue("limit"), 10, 64)
	params.Timeout, _ = strconv.ParseUint(r.FormValue("timeout"), 10, 64)

	// Alternatively, Telegram Bot API supports submitting request through JSON
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct == "application/json" {
		_ = json.NewDecoder(r.Body).Decode(&params)
	}

	// Since we don't delete updates from the database, when offset=0, we return an empty update for the client to poll again with a new offset value.
	if params.Offset == 0 {
		lastUpdateID, err := s.db.GetLastUpdateID(r.Context())
		if err != nil {
			s.internalServerErrorHandler(w, err)
			return
		}
		h := w.Header()
		h.Set("Cache-Control", "no-cache")
		h.Set("Content-Type", "application/json")
		h.Set("X-Content-Type-Options", "nosniff")
		fmt.Fprintf(w, "{\"ok\":true,\"result\":[{\"update_id\":%d}]}", lastUpdateID)
		return
	}

	// Limit parameter range
	if params.Limit == 0 || params.Limit > 100 {
		params.Limit = 100
	}
	if params.Timeout <= 0 {
		params.Timeout = 0
	}

	timer := time.After(time.Duration(params.Timeout) * time.Second)
	for {
		update, cancel := s.db.SubscribeNextUpdate()
		updatesReceived := false
		for updateJSON, err := range s.db.GetUpdates(r.Context(), params.Offset, params.Limit) {
			if err != nil {
				cancel()
				s.internalServerErrorHandler(w, err)
				return
			}
			if !updatesReceived {
				h := w.Header()
				h.Set("Cache-Control", "no-cache")
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
			h.Set("Cache-Control", "no-cache")
			h.Set("Content-Type", "application/json")
			h.Set("X-Content-Type-Options", "nosniff")
			w.Write([]byte("{\"ok\":true,\"result\":[]}"))
			return
		case <-update:
		}
	}
}

func (s *Server) forwardAPI(w http.ResponseWriter, r *http.Request, funcName string) {
	var bodyCopy io.ReadCloser
	r.Body, bodyCopy = NewPreserveBodyReader(r.Body)

	err := s.c.ForwardRequest(r.Context(), s, w, r, false, funcName, bodyCopy)
	if err != nil {
		s.internalServerErrorHandler(w, err)
	}
}

func (s *Server) forwardFileRequest(w http.ResponseWriter, r *http.Request, fileID string) {
	err := s.c.ForwardRequest(r.Context(), s, w, r, true, fileID, r.Body)
	if err != nil {
		s.internalServerErrorHandler(w, err)
	}
}

func (s *Server) ReportError(w http.ResponseWriter, code int) {
	h := w.Header()
	h.Del("Content-Length")
	h.Set("Cache-Control", "no-cache")
	h.Set("Content-Type", "application/json")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	fmt.Fprintf(w, "{\"ok\":false,\"error_code\":%d,\"description\":%s}", code, http.StatusText(code))
}

func (s *Server) internalServerErrorHandler(w http.ResponseWriter, err error) {
	debug.PrintStack()
	log.Println("Error:", err)
	s.ReportError(w, http.StatusInternalServerError)
}
