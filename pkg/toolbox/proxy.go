// Copyright 2026 SandrPod Contributors
// Port preview proxy: forwards /proxy/{port}/{path} to a service listening on
// localhost inside the sandbox, so web apps started by an agent are reachable
// through the platform (server → tunnel → poder → toolbox → localhost:port).

package toolbox

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
)

// proxyPortHandler handles /proxy/{port}/{rest...} by reverse-proxying to
// http://127.0.0.1:{port}/{rest...} (query string preserved).
func (s *Server) proxyPortHandler(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/proxy/")
	parts := strings.SplitN(trimmed, "/", 2)
	port, err := strconv.Atoi(parts[0])
	if err != nil || port < 1 || port > 65535 {
		http.Error(w, "invalid port in /proxy/{port}/...", http.StatusBadRequest)
		return
	}
	rest := "/"
	if len(parts) == 2 {
		rest = "/" + parts[1]
	}

	target := &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", port)}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, fmt.Sprintf("no service reachable on port %d: %v", port, err), http.StatusBadGateway)
	}

	r.URL.Path = rest
	r.URL.RawPath = ""
	r.Host = target.Host
	proxy.ServeHTTP(w, r)
}
