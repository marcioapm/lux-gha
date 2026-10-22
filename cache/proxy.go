package cache

import (
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
)

// newProxy forwards a request to the fixed upstream with its method, path,
// query, headers (Authorization included) and body, and streams the
// response back. Only the scheme and host change; the upstream is never
// derived from the request.
func newProxy(upstream *url.URL, log *slog.Logger) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream)
			// SetURL joins the upstream path; an upstream with no path
			// leaves the request's path, raw encoding included, as it was.
		},
		// Flush every write: artifact downloads and long polls stream.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Warn("cache: upstream request failed", "method", r.Method, "path", r.URL.Path, "error", err)
			w.WriteHeader(http.StatusBadGateway)
		},
	}
}
