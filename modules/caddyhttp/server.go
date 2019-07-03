// Copyright 2015 Matthew Holt and The Caddy Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package caddyhttp

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddytls"
)

// Server is an HTTP server.
type Server struct {
	Listen            []string                    `json:"listen,omitempty"`
	ReadTimeout       caddy.Duration              `json:"read_timeout,omitempty"`
	ReadHeaderTimeout caddy.Duration              `json:"read_header_timeout,omitempty"`
	WriteTimeout      caddy.Duration              `json:"write_timeout,omitempty"`
	IdleTimeout       caddy.Duration              `json:"idle_timeout,omitempty"`
	MaxHeaderBytes    int                         `json:"max_header_bytes,omitempty"`
	Routes            RouteList                   `json:"routes,omitempty"`
	Errors            *HTTPErrorConfig            `json:"errors,omitempty"`
	TLSConnPolicies   caddytls.ConnectionPolicies `json:"tls_connection_policies,omitempty"`
	AutoHTTPS         *AutoHTTPSConfig            `json:"automatic_https,omitempty"`
	MaxRehandles      int                         `json:"max_rehandles,omitempty"`
	StrictSNIHost     bool                        `json:"strict_sni_host,omitempty"` // TODO: see if we can turn this on by default when clientauth is configured

	tlsApp *caddytls.TLS
}

// ServeHTTP is the entry point for all HTTP requests.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Server", "Caddy")

	if s.tlsApp.HandleHTTPChallenge(w, r) {
		return
	}

	// set up the context for the request
	repl := caddy.NewReplacer()
	ctx := context.WithValue(r.Context(), caddy.ReplacerCtxKey, repl)
	ctx = context.WithValue(ctx, ServerCtxKey, s)
	ctx = context.WithValue(ctx, TableCtxKey, make(map[string]interface{})) // TODO: Implement this
	r = r.WithContext(ctx)

	// once the pointer to the request won't change
	// anymore, finish setting up the replacer
	addHTTPVarsToReplacer(repl, r, w)

	// build and execute the main handler chain
	stack, wrappedWriter := s.Routes.BuildCompositeRoute(w, r)
	stack = s.wrapPrimaryRoute(stack)
	err := s.executeCompositeRoute(wrappedWriter, r, stack)
	if err != nil {
		// add the raw error value to the request context
		// so it can be accessed by error handlers
		c := context.WithValue(r.Context(), ErrorCtxKey, err)
		r = r.WithContext(c)

		// add error values to the replacer
		repl.Set("http.error", err.Error())
		if handlerErr, ok := err.(HandlerError); ok {
			repl.Set("http.error.status_code", strconv.Itoa(handlerErr.StatusCode))
			repl.Set("http.error.status_text", http.StatusText(handlerErr.StatusCode))
			repl.Set("http.error.message", handlerErr.Message)
			repl.Set("http.error.trace", handlerErr.Trace)
			repl.Set("http.error.id", handlerErr.ID)
		}

		if s.Errors != nil && len(s.Errors.Routes) > 0 {
			errStack, wrappedWriter := s.Errors.Routes.BuildCompositeRoute(w, r)
			err := s.executeCompositeRoute(wrappedWriter, r, errStack)
			if err != nil {
				// TODO: what should we do if the error handler has an error?
				log.Printf("[ERROR] [%s %s] handling error: %v", r.Method, r.RequestURI, err)
			}
		} else {
			// TODO: polish the default error handling
			log.Printf("[ERROR] [%s %s] %v", r.Method, r.RequestURI, err)
			if handlerErr, ok := err.(HandlerError); ok {
				w.WriteHeader(handlerErr.StatusCode)
			} else {
				w.WriteHeader(http.StatusInternalServerError)
			}
		}
	}
}

// executeCompositeRoute executes stack with w and r. This function handles
// the special ErrRehandle error value, which reprocesses requests through
// the stack again. Any error value returned from this function would be an
// actual error that needs to be handled.
func (s *Server) executeCompositeRoute(w http.ResponseWriter, r *http.Request, stack Handler) error {
	var err error
	for i := -1; i <= s.MaxRehandles; i++ {
		// we started the counter at -1 because we
		// always want to run this at least once
		err = stack.ServeHTTP(w, r)
		if err != ErrRehandle {
			break
		}
		if i >= s.MaxRehandles-1 {
			return fmt.Errorf("too many rehandles")
		}
	}
	return err
}

// wrapPrimaryRoute wraps stack (a compiled middleware handler chain)
// in s.enforcementHandler which performs crucial security checks, etc.
func (s *Server) wrapPrimaryRoute(stack Handler) Handler {
	return HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		return s.enforcementHandler(w, r, stack)
	})
}

// enforcementHandler is an implicit middleware which performs
// standard checks before executing the HTTP middleware chain.
func (s *Server) enforcementHandler(w http.ResponseWriter, r *http.Request, next Handler) error {
	// enforce strict host matching, which ensures that the SNI
	// value (if any), matches the Host header; essential for
	// servers that rely on TLS ClientAuth sharing a listener
	// with servers that do not; if not enforced, client could
	// bypass by sending benign SNI then restricted Host header
	if s.StrictSNIHost && r.TLS != nil {
		hostname, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			hostname = r.Host // OK; probably lacked port
		}
		if strings.ToLower(r.TLS.ServerName) != strings.ToLower(hostname) {
			err := fmt.Errorf("strict host matching: TLS ServerName (%s) and HTTP Host (%s) values differ",
				r.TLS.ServerName, hostname)
			r.Close = true
			return Error(http.StatusForbidden, err)
		}
	}
	return next.ServeHTTP(w, r)
}

func (s *Server) listenersUseAnyPortOtherThan(otherPort int) bool {
	for _, lnAddr := range s.Listen {
		_, addrs, err := parseListenAddr(lnAddr)
		if err == nil {
			for _, a := range addrs {
				_, port, err := net.SplitHostPort(a)
				if err == nil && port != strconv.Itoa(otherPort) {
					return true
				}
			}
		}
	}
	return false
}

// AutoHTTPSConfig is used to disable automatic HTTPS
// or certain aspects of it for a specific server.
type AutoHTTPSConfig struct {
	// If true, automatic HTTPS will be entirely disabled.
	Disabled bool `json:"disable,omitempty"`

	// If true, only automatic HTTP->HTTPS redirects will
	// be disabled.
	DisableRedir bool `json:"disable_redirects,omitempty"`

	// Hosts/domain names listed here will not be included
	// in automatic HTTPS (they will not have certificates
	// loaded nor redirects applied).
	Skip []string `json:"skip,omitempty"`

	// Hosts/domain names listed here will still be enabled
	// for automatic HTTPS (unless in the Skip list), except
	// that certificates will not be provisioned and managed
	// for these names.
	SkipCerts []string `json:"skip_certificates,omitempty"`
}

// Skipped returns true if name is in skipSlice, which
// should be one of the Skip* fields on ahc.
func (ahc AutoHTTPSConfig) Skipped(name string, skipSlice []string) bool {
	for _, n := range skipSlice {
		if name == n {
			return true
		}
	}
	return false
}

// HTTPErrorConfig determines how to handle errors
// from the HTTP handlers.
type HTTPErrorConfig struct {
	Routes RouteList `json:"routes,omitempty"`
}

// Context keys for HTTP request context values.
const (
	// For referencing the server instance
	ServerCtxKey caddy.CtxKey = "server"

	// For the request's variable table (TODO: implement this)
	TableCtxKey caddy.CtxKey = "table"
)