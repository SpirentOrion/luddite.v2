package luddite

import (
	"math"
	"net/http"
	"net/url"
	"strconv"
)

const (
	HeaderAccept                 = "Accept"
	HeaderAcceptEncoding         = "Accept-Encoding"
	HeaderAuthorization          = "Authorization"
	HeaderCacheControl           = "Cache-Control"
	HeaderContentDisposition     = "Content-Disposition"
	HeaderContentEncoding        = "Content-Encoding"
	HeaderContentLength          = "Content-Length"
	HeaderContentType            = "Content-Type"
	HeaderETag                   = "ETag"
	HeaderExpect                 = "Expect"
	HeaderForwardedFor           = "X-Forwarded-For"
	HeaderForwardedHost          = "X-Forwarded-Host"
	HeaderIfNoneMatch            = "If-None-Match"
	HeaderLocation               = "Location"
	HeaderRequestId              = "X-Request-Id"
	HeaderSessionId              = "X-Session-Id"
	HeaderSpirentApiVersion      = "X-Spirent-Api-Version"
	HeaderSpirentInhibitResponse = "X-Spirent-Inhibit-Response"
	HeaderSpirentNextLink        = "X-Spirent-Next-Link"
	HeaderSpirentPageSize        = "X-Spirent-Page-Size"
	HeaderSpirentResourceNonce   = "X-Spirent-Resource-Nonce"
	HeaderUserAgent              = "User-Agent"
)

func RequestBearerToken(r *http.Request) string {
	if s := r.Header.Get(HeaderAuthorization); len(s) >= 7 && s[:7] == "Bearer " {
		return s[7:]
	}
	return r.URL.Query().Get("access_token")
}

func RequestExternalHost(r *http.Request) string {
	if host := r.Header.Get(HeaderForwardedHost); host != "" {
		return host
	}
	return r.Host
}

func RequestNextLink(r *http.Request, cursor string) *url.URL {
	next := *r.URL
	v := next.Query()
	v.Set("cursor", cursor)
	next.RawQuery = v.Encode()
	return &next
}

func RequestPageSize(r *http.Request) (pageSize int) {
	var err error
	if pageSize, err = strconv.Atoi(r.Header.Get(HeaderSpirentPageSize)); err != nil || pageSize <= 0 {
		pageSize = math.MaxInt32
	}
	return
}

func RequestQueryCursor(r *http.Request) string {
	return r.URL.Query().Get("cursor")
}

func RequestResourceNonce(r *http.Request) string {
	return r.Header.Get(HeaderSpirentResourceNonce)
}
