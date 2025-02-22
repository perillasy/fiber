// ⚡️ Fiber is an Express inspired web framework written in Go with ☕️
// 🤖 Github Repository: https://github.com/gofiber/fiber
// 📌 API Documentation: https://docs.gofiber.io

package fiber

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
	"unsafe"

	"github.com/gofiber/fiber/v2/internal/bytebufferpool"
	"github.com/gofiber/fiber/v2/utils"
	"github.com/valyala/fasthttp"
)

/* #nosec */
// lnMetadata will close the listener and return the addr and tls config
func lnMetadata(network string, ln net.Listener) (addr string, cfg *tls.Config) {
	// Get addr
	addr = ln.Addr().String()

	// Close listener
	if err := ln.Close(); err != nil {
		return
	}

	// Wait for the listener to be closed
	var closed bool
	for i := 0; i < 10; i++ {
		conn, err := net.DialTimeout(network, addr, 3*time.Second)
		if err != nil || conn == nil {
			closed = true
			break
		}
		_ = conn.Close()
		time.Sleep(100 * time.Millisecond)
	}
	if !closed {
		panic("listener: " + addr + ": Only one usage of each socket address (protocol/network address/port) is normally permitted.")
	}

	cfg = getTlsConfig(ln)

	return
}

/* #nosec */
// getTlsConfig returns a net listener's tls config
func getTlsConfig(ln net.Listener) *tls.Config {
	// Get listener type
	pointer := reflect.ValueOf(ln)

	// Is it a tls.listener?
	if pointer.String() == "<*tls.listener Value>" {
		// Copy value from pointer
		if val := reflect.Indirect(pointer); val.Type() != nil {
			// Get private field from value
			if field := val.FieldByName("config"); field.Type() != nil {
				// Copy value from pointer field (unsafe)
				newval := reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())) // #nosec G103
				if newval.Type() != nil {
					// Get element from pointer
					if elem := newval.Elem(); elem.Type() != nil {
						// Cast value to *tls.Config
						return elem.Interface().(*tls.Config)
					}
				}
			}
		}
	}

	return nil
}

// readContent opens a named file and read content from it
func readContent(rf io.ReaderFrom, name string) (n int64, err error) {
	// Read file
	f, err := os.Open(filepath.Clean(name))
	if err != nil {
		return 0, err
	}
	// #nosec G307
	defer func() {
		if err = f.Close(); err != nil {
			log.Printf("Error closing file: %s\n", err)
		}
	}()
	return rf.ReadFrom(f)
}

// quoteString escape special characters in a given string
func (app *App) quoteString(raw string) string {
	bb := bytebufferpool.Get()
	// quoted := string(fasthttp.AppendQuotedArg(bb.B, getBytes(raw)))
	quoted := app.getString(fasthttp.AppendQuotedArg(bb.B, app.getBytes(raw)))
	bytebufferpool.Put(bb)
	return quoted
}

// Scan stack if other methods match the request
func methodExist(ctx *Ctx) (exist bool) {
	for i := 0; i < len(intMethod); i++ {
		// Skip original method
		if ctx.methodINT == i {
			continue
		}
		// Reset stack index
		ctx.indexRoute = -1
		tree, ok := ctx.app.treeStack[i][ctx.treePath]
		if !ok {
			tree = ctx.app.treeStack[i][""]
		}
		// Get stack length
		lenr := len(tree) - 1
		// Loop over the route stack starting from previous index
		for ctx.indexRoute < lenr {
			// Increment route index
			ctx.indexRoute++
			// Get *Route
			route := tree[ctx.indexRoute]
			// Skip use routes
			if route.use {
				continue
			}
			// Check if it matches the request path
			match := route.match(ctx.detectionPath, ctx.path, &ctx.values)
			// No match, next route
			if match {
				// We matched
				exist = true
				// Add method to Allow header
				ctx.Append(HeaderAllow, intMethod[i])
				// Break stack loop
				break
			}
		}
	}
	return
}

// uniqueRouteStack drop all not unique routes from the slice
func uniqueRouteStack(stack []*Route) []*Route {
	var unique []*Route
	m := make(map[*Route]int)
	for _, v := range stack {
		if _, ok := m[v]; !ok {
			// Unique key found. Record position and collect
			// in result.
			m[v] = len(unique)
			unique = append(unique, v)
		}
	}

	return unique
}

// defaultString returns the value or a default value if it is set
func defaultString(value string, defaultValue []string) string {
	if len(value) == 0 && len(defaultValue) > 0 {
		return defaultValue[0]
	}
	return value
}

const normalizedHeaderETag = "Etag"

// Generate and set ETag header to response
func setETag(c *Ctx, weak bool) {
	// Don't generate ETags for invalid responses
	if c.fasthttp.Response.StatusCode() != StatusOK {
		return
	}
	body := c.fasthttp.Response.Body()
	// Skips ETag if no response body is present
	if len(body) == 0 {
		return
	}
	// Get ETag header from request
	clientEtag := c.Get(HeaderIfNoneMatch)

	// Generate ETag for response
	crc32q := crc32.MakeTable(0xD5828281)
	etag := fmt.Sprintf("\"%d-%v\"", len(body), crc32.Checksum(body, crc32q))

	// Enable weak tag
	if weak {
		etag = "W/" + etag
	}

	// Check if client's ETag is weak
	if strings.HasPrefix(clientEtag, "W/") {
		// Check if server's ETag is weak
		if clientEtag[2:] == etag || clientEtag[2:] == etag[2:] {
			// W/1 == 1 || W/1 == W/1
			_ = c.SendStatus(StatusNotModified)
			c.fasthttp.ResetBody()
			return
		}
		// W/1 != W/2 || W/1 != 2
		c.setCanonical(normalizedHeaderETag, etag)
		return
	}
	if strings.Contains(clientEtag, etag) {
		// 1 == 1
		_ = c.SendStatus(StatusNotModified)
		c.fasthttp.ResetBody()
		return
	}
	// 1 != 2
	c.setCanonical(normalizedHeaderETag, etag)
}

func getGroupPath(prefix, path string) string {
	if len(path) == 0 || path == "/" {
		return prefix
	}

	if path[0] != '/' {
		path = "/" + path
	}

	return utils.TrimRight(prefix, '/') + path
}

// return valid offer for header negotiation
func getOffer(header string, offers ...string) string {
	if len(offers) == 0 {
		return ""
	} else if header == "" {
		return offers[0]
	}

	spec, commaPos := "", 0
	for len(header) > 0 && commaPos != -1 {
		commaPos = strings.IndexByte(header, ',')
		if commaPos != -1 {
			spec = utils.Trim(header[:commaPos], ' ')
		} else {
			spec = header
		}
		if factorSign := strings.IndexByte(spec, ';'); factorSign != -1 {
			spec = spec[:factorSign]
		}

		for _, offer := range offers {
			// has star prefix
			if len(spec) >= 1 && spec[len(spec)-1] == '*' {
				return offer
			} else if strings.HasPrefix(spec, offer) {
				return offer
			}
		}
		if commaPos != -1 {
			header = header[commaPos+1:]
		}
	}

	return ""
}

func matchEtag(s string, etag string) bool {
	if s == etag || s == "W/"+etag || "W/"+s == etag {
		return true
	}

	return false
}

func (app *App) isEtagStale(etag string, noneMatchBytes []byte) bool {
	var start, end int

	// Adapted from:
	// https://github.com/jshttp/fresh/blob/10e0471669dbbfbfd8de65bc6efac2ddd0bfa057/index.js#L110
	for i := range noneMatchBytes {
		switch noneMatchBytes[i] {
		case 0x20:
			if start == end {
				start = i + 1
				end = i + 1
			}
		case 0x2c:
			if matchEtag(app.getString(noneMatchBytes[start:end]), etag) {
				return false
			}
			start = i + 1
			end = i + 1
		default:
			end = i + 1
		}
	}

	return !matchEtag(app.getString(noneMatchBytes[start:end]), etag)
}

func parseAddr(raw string) (host, port string) {
	if i := strings.LastIndex(raw, ":"); i != -1 {
		return raw[:i], raw[i+1:]
	}
	return raw, ""
}

const noCacheValue = "no-cache"

// isNoCache checks if the cacheControl header value is a `no-cache`.
func isNoCache(cacheControl string) bool {
	i := strings.Index(cacheControl, noCacheValue)
	if i == -1 {
		return false
	}

	// Xno-cache
	if i > 0 && !(cacheControl[i-1] == ' ' || cacheControl[i-1] == ',') {
		return false
	}

	// bla bla, no-cache
	if i+len(noCacheValue) == len(cacheControl) {
		return true
	}

	// bla bla, no-cacheX
	if cacheControl[i+len(noCacheValue)] != ',' {
		return false
	}

	// OK
	return true
}

type testConn struct {
	r bytes.Buffer
	w bytes.Buffer
}

func (c *testConn) Read(b []byte) (int, error)  { return c.r.Read(b) }
func (c *testConn) Write(b []byte) (int, error) { return c.w.Write(b) }
func (c *testConn) Close() error                { return nil }

func (c *testConn) LocalAddr() net.Addr                { return &net.TCPAddr{Port: 0, Zone: "", IP: net.IPv4zero} }
func (c *testConn) RemoteAddr() net.Addr               { return &net.TCPAddr{Port: 0, Zone: "", IP: net.IPv4zero} }
func (c *testConn) SetDeadline(_ time.Time) error      { return nil }
func (c *testConn) SetReadDeadline(_ time.Time) error  { return nil }
func (c *testConn) SetWriteDeadline(_ time.Time) error { return nil }

var getStringImmutable = func(b []byte) string {
	return string(b)
}

var getBytesImmutable = func(s string) (b []byte) {
	return []byte(s)
}

// HTTP methods and their unique INTs
func methodInt(s string) int {
	switch s {
	case MethodGet:
		return 0
	case MethodHead:
		return 1
	case MethodPost:
		return 2
	case MethodPut:
		return 3
	case MethodDelete:
		return 4
	case MethodConnect:
		return 5
	case MethodOptions:
		return 6
	case MethodTrace:
		return 7
	case MethodPatch:
		return 8
	default:
		return -1
	}
}

// HTTP methods slice
var intMethod = []string{
	MethodGet,
	MethodHead,
	MethodPost,
	MethodPut,
	MethodDelete,
	MethodConnect,
	MethodOptions,
	MethodTrace,
	MethodPatch,
}

// HTTP methods were copied from net/http.
const (
	MethodGet     = "GET"     // RFC 7231, 4.3.1
	MethodHead    = "HEAD"    // RFC 7231, 4.3.2
	MethodPost    = "POST"    // RFC 7231, 4.3.3
	MethodPut     = "PUT"     // RFC 7231, 4.3.4
	MethodPatch   = "PATCH"   // RFC 5789
	MethodDelete  = "DELETE"  // RFC 7231, 4.3.5
	MethodConnect = "CONNECT" // RFC 7231, 4.3.6
	MethodOptions = "OPTIONS" // RFC 7231, 4.3.7
	MethodTrace   = "TRACE"   // RFC 7231, 4.3.8
	methodUse     = "USE"
)

// MIME types that are commonly used
const (
	MIMETextXML               = "text/xml"
	MIMETextHTML              = "text/html"
	MIMETextPlain             = "text/plain"
	MIMEApplicationXML        = "application/xml"
	MIMEApplicationJSON       = "application/json"
	MIMEApplicationJavaScript = "application/javascript"
	MIMEApplicationForm       = "application/x-www-form-urlencoded"
	MIMEOctetStream           = "application/octet-stream"
	MIMEMultipartForm         = "multipart/form-data"

	MIMETextXMLCharsetUTF8               = "text/xml; charset=utf-8"
	MIMETextHTMLCharsetUTF8              = "text/html; charset=utf-8"
	MIMETextPlainCharsetUTF8             = "text/plain; charset=utf-8"
	MIMEApplicationXMLCharsetUTF8        = "application/xml; charset=utf-8"
	MIMEApplicationJSONCharsetUTF8       = "application/json; charset=utf-8"
	MIMEApplicationJavaScriptCharsetUTF8 = "application/javascript; charset=utf-8"
)

// HTTP status codes were copied from net/http.
const (
	StatusContinue                      = 100 // RFC 7231, 6.2.1
	StatusSwitchingProtocols            = 101 // RFC 7231, 6.2.2
	StatusProcessing                    = 102 // RFC 2518, 10.1
	StatusEarlyHints                    = 103 // RFC 8297
	StatusOK                            = 200 // RFC 7231, 6.3.1
	StatusCreated                       = 201 // RFC 7231, 6.3.2
	StatusAccepted                      = 202 // RFC 7231, 6.3.3
	StatusNonAuthoritativeInformation   = 203 // RFC 7231, 6.3.4
	StatusNoContent                     = 204 // RFC 7231, 6.3.5
	StatusResetContent                  = 205 // RFC 7231, 6.3.6
	StatusPartialContent                = 206 // RFC 7233, 4.1
	StatusMultiStatus                   = 207 // RFC 4918, 11.1
	StatusAlreadyReported               = 208 // RFC 5842, 7.1
	StatusIMUsed                        = 226 // RFC 3229, 10.4.1
	StatusMultipleChoices               = 300 // RFC 7231, 6.4.1
	StatusMovedPermanently              = 301 // RFC 7231, 6.4.2
	StatusFound                         = 302 // RFC 7231, 6.4.3
	StatusSeeOther                      = 303 // RFC 7231, 6.4.4
	StatusNotModified                   = 304 // RFC 7232, 4.1
	StatusUseProxy                      = 305 // RFC 7231, 6.4.5
	StatusTemporaryRedirect             = 307 // RFC 7231, 6.4.7
	StatusPermanentRedirect             = 308 // RFC 7538, 3
	StatusBadRequest                    = 400 // RFC 7231, 6.5.1
	StatusUnauthorized                  = 401 // RFC 7235, 3.1
	StatusPaymentRequired               = 402 // RFC 7231, 6.5.2
	StatusForbidden                     = 403 // RFC 7231, 6.5.3
	StatusNotFound                      = 404 // RFC 7231, 6.5.4
	StatusMethodNotAllowed              = 405 // RFC 7231, 6.5.5
	StatusNotAcceptable                 = 406 // RFC 7231, 6.5.6
	StatusProxyAuthRequired             = 407 // RFC 7235, 3.2
	StatusRequestTimeout                = 408 // RFC 7231, 6.5.7
	StatusConflict                      = 409 // RFC 7231, 6.5.8
	StatusGone                          = 410 // RFC 7231, 6.5.9
	StatusLengthRequired                = 411 // RFC 7231, 6.5.10
	StatusPreconditionFailed            = 412 // RFC 7232, 4.2
	StatusRequestEntityTooLarge         = 413 // RFC 7231, 6.5.11
	StatusRequestURITooLong             = 414 // RFC 7231, 6.5.12
	StatusUnsupportedMediaType          = 415 // RFC 7231, 6.5.13
	StatusRequestedRangeNotSatisfiable  = 416 // RFC 7233, 4.4
	StatusExpectationFailed             = 417 // RFC 7231, 6.5.14
	StatusTeapot                        = 418 // RFC 7168, 2.3.3
	StatusMisdirectedRequest            = 421 // RFC 7540, 9.1.2
	StatusUnprocessableEntity           = 422 // RFC 4918, 11.2
	StatusLocked                        = 423 // RFC 4918, 11.3
	StatusFailedDependency              = 424 // RFC 4918, 11.4
	StatusTooEarly                      = 425 // RFC 8470, 5.2.
	StatusUpgradeRequired               = 426 // RFC 7231, 6.5.15
	StatusPreconditionRequired          = 428 // RFC 6585, 3
	StatusTooManyRequests               = 429 // RFC 6585, 4
	StatusRequestHeaderFieldsTooLarge   = 431 // RFC 6585, 5
	StatusUnavailableForLegalReasons    = 451 // RFC 7725, 3
	StatusInternalServerError           = 500 // RFC 7231, 6.6.1
	StatusNotImplemented                = 501 // RFC 7231, 6.6.2
	StatusBadGateway                    = 502 // RFC 7231, 6.6.3
	StatusServiceUnavailable            = 503 // RFC 7231, 6.6.4
	StatusGatewayTimeout                = 504 // RFC 7231, 6.6.5
	StatusHTTPVersionNotSupported       = 505 // RFC 7231, 6.6.6
	StatusVariantAlsoNegotiates         = 506 // RFC 2295, 8.1
	StatusInsufficientStorage           = 507 // RFC 4918, 11.5
	StatusLoopDetected                  = 508 // RFC 5842, 7.2
	StatusNotExtended                   = 510 // RFC 2774, 7
	StatusNetworkAuthenticationRequired = 511 // RFC 6585, 6
)

// Errors
var (
	ErrBadRequest                    = NewError(StatusBadRequest)                    // RFC 7231, 6.5.1
	ErrUnauthorized                  = NewError(StatusUnauthorized)                  // RFC 7235, 3.1
	ErrPaymentRequired               = NewError(StatusPaymentRequired)               // RFC 7231, 6.5.2
	ErrForbidden                     = NewError(StatusForbidden)                     // RFC 7231, 6.5.3
	ErrNotFound                      = NewError(StatusNotFound)                      // RFC 7231, 6.5.4
	ErrMethodNotAllowed              = NewError(StatusMethodNotAllowed)              // RFC 7231, 6.5.5
	ErrNotAcceptable                 = NewError(StatusNotAcceptable)                 // RFC 7231, 6.5.6
	ErrProxyAuthRequired             = NewError(StatusProxyAuthRequired)             // RFC 7235, 3.2
	ErrRequestTimeout                = NewError(StatusRequestTimeout)                // RFC 7231, 6.5.7
	ErrConflict                      = NewError(StatusConflict)                      // RFC 7231, 6.5.8
	ErrGone                          = NewError(StatusGone)                          // RFC 7231, 6.5.9
	ErrLengthRequired                = NewError(StatusLengthRequired)                // RFC 7231, 6.5.10
	ErrPreconditionFailed            = NewError(StatusPreconditionFailed)            // RFC 7232, 4.2
	ErrRequestEntityTooLarge         = NewError(StatusRequestEntityTooLarge)         // RFC 7231, 6.5.11
	ErrRequestURITooLong             = NewError(StatusRequestURITooLong)             // RFC 7231, 6.5.12
	ErrUnsupportedMediaType          = NewError(StatusUnsupportedMediaType)          // RFC 7231, 6.5.13
	ErrRequestedRangeNotSatisfiable  = NewError(StatusRequestedRangeNotSatisfiable)  // RFC 7233, 4.4
	ErrExpectationFailed             = NewError(StatusExpectationFailed)             // RFC 7231, 6.5.14
	ErrTeapot                        = NewError(StatusTeapot)                        // RFC 7168, 2.3.3
	ErrMisdirectedRequest            = NewError(StatusMisdirectedRequest)            // RFC 7540, 9.1.2
	ErrUnprocessableEntity           = NewError(StatusUnprocessableEntity)           // RFC 4918, 11.2
	ErrLocked                        = NewError(StatusLocked)                        // RFC 4918, 11.3
	ErrFailedDependency              = NewError(StatusFailedDependency)              // RFC 4918, 11.4
	ErrTooEarly                      = NewError(StatusTooEarly)                      // RFC 8470, 5.2.
	ErrUpgradeRequired               = NewError(StatusUpgradeRequired)               // RFC 7231, 6.5.15
	ErrPreconditionRequired          = NewError(StatusPreconditionRequired)          // RFC 6585, 3
	ErrTooManyRequests               = NewError(StatusTooManyRequests)               // RFC 6585, 4
	ErrRequestHeaderFieldsTooLarge   = NewError(StatusRequestHeaderFieldsTooLarge)   // RFC 6585, 5
	ErrUnavailableForLegalReasons    = NewError(StatusUnavailableForLegalReasons)    // RFC 7725, 3
	ErrInternalServerError           = NewError(StatusInternalServerError)           // RFC 7231, 6.6.1
	ErrNotImplemented                = NewError(StatusNotImplemented)                // RFC 7231, 6.6.2
	ErrBadGateway                    = NewError(StatusBadGateway)                    // RFC 7231, 6.6.3
	ErrServiceUnavailable            = NewError(StatusServiceUnavailable)            // RFC 7231, 6.6.4
	ErrGatewayTimeout                = NewError(StatusGatewayTimeout)                // RFC 7231, 6.6.5
	ErrHTTPVersionNotSupported       = NewError(StatusHTTPVersionNotSupported)       // RFC 7231, 6.6.6
	ErrVariantAlsoNegotiates         = NewError(StatusVariantAlsoNegotiates)         // RFC 2295, 8.1
	ErrInsufficientStorage           = NewError(StatusInsufficientStorage)           // RFC 4918, 11.5
	ErrLoopDetected                  = NewError(StatusLoopDetected)                  // RFC 5842, 7.2
	ErrNotExtended                   = NewError(StatusNotExtended)                   // RFC 2774, 7
	ErrNetworkAuthenticationRequired = NewError(StatusNetworkAuthenticationRequired) // RFC 6585, 6
)

// HTTP Headers were copied from net/http.
const (
	HeaderAuthorization                   = "Authorization"
	HeaderProxyAuthenticate               = "Proxy-Authenticate"
	HeaderProxyAuthorization              = "Proxy-Authorization"
	HeaderWWWAuthenticate                 = "WWW-Authenticate"
	HeaderAge                             = "Age"
	HeaderCacheControl                    = "Cache-Control"
	HeaderClearSiteData                   = "Clear-Site-Data"
	HeaderExpires                         = "Expires"
	HeaderPragma                          = "Pragma"
	HeaderWarning                         = "Warning"
	HeaderAcceptCH                        = "Accept-CH"
	HeaderAcceptCHLifetime                = "Accept-CH-Lifetime"
	HeaderContentDPR                      = "Content-DPR"
	HeaderDPR                             = "DPR"
	HeaderEarlyData                       = "Early-Data"
	HeaderSaveData                        = "Save-Data"
	HeaderViewportWidth                   = "Viewport-Width"
	HeaderWidth                           = "Width"
	HeaderETag                            = "ETag"
	HeaderIfMatch                         = "If-Match"
	HeaderIfModifiedSince                 = "If-Modified-Since"
	HeaderIfNoneMatch                     = "If-None-Match"
	HeaderIfUnmodifiedSince               = "If-Unmodified-Since"
	HeaderLastModified                    = "Last-Modified"
	HeaderVary                            = "Vary"
	HeaderConnection                      = "Connection"
	HeaderKeepAlive                       = "Keep-Alive"
	HeaderAccept                          = "Accept"
	HeaderAcceptCharset                   = "Accept-Charset"
	HeaderAcceptEncoding                  = "Accept-Encoding"
	HeaderAcceptLanguage                  = "Accept-Language"
	HeaderCookie                          = "Cookie"
	HeaderExpect                          = "Expect"
	HeaderMaxForwards                     = "Max-Forwards"
	HeaderSetCookie                       = "Set-Cookie"
	HeaderAccessControlAllowCredentials   = "Access-Control-Allow-Credentials"
	HeaderAccessControlAllowHeaders       = "Access-Control-Allow-Headers"
	HeaderAccessControlAllowMethods       = "Access-Control-Allow-Methods"
	HeaderAccessControlAllowOrigin        = "Access-Control-Allow-Origin"
	HeaderAccessControlExposeHeaders      = "Access-Control-Expose-Headers"
	HeaderAccessControlMaxAge             = "Access-Control-Max-Age"
	HeaderAccessControlRequestHeaders     = "Access-Control-Request-Headers"
	HeaderAccessControlRequestMethod      = "Access-Control-Request-Method"
	HeaderOrigin                          = "Origin"
	HeaderTimingAllowOrigin               = "Timing-Allow-Origin"
	HeaderXPermittedCrossDomainPolicies   = "X-Permitted-Cross-Domain-Policies"
	HeaderDNT                             = "DNT"
	HeaderTk                              = "Tk"
	HeaderContentDisposition              = "Content-Disposition"
	HeaderContentEncoding                 = "Content-Encoding"
	HeaderContentLanguage                 = "Content-Language"
	HeaderContentLength                   = "Content-Length"
	HeaderContentLocation                 = "Content-Location"
	HeaderContentType                     = "Content-Type"
	HeaderForwarded                       = "Forwarded"
	HeaderVia                             = "Via"
	HeaderXForwardedFor                   = "X-Forwarded-For"
	HeaderXForwardedHost                  = "X-Forwarded-Host"
	HeaderXForwardedProto                 = "X-Forwarded-Proto"
	HeaderXForwardedProtocol              = "X-Forwarded-Protocol"
	HeaderXForwardedSsl                   = "X-Forwarded-Ssl"
	HeaderXUrlScheme                      = "X-Url-Scheme"
	HeaderLocation                        = "Location"
	HeaderFrom                            = "From"
	HeaderHost                            = "Host"
	HeaderReferer                         = "Referer"
	HeaderReferrerPolicy                  = "Referrer-Policy"
	HeaderUserAgent                       = "User-Agent"
	HeaderAllow                           = "Allow"
	HeaderServer                          = "Server"
	HeaderAcceptRanges                    = "Accept-Ranges"
	HeaderContentRange                    = "Content-Range"
	HeaderIfRange                         = "If-Range"
	HeaderRange                           = "Range"
	HeaderContentSecurityPolicy           = "Content-Security-Policy"
	HeaderContentSecurityPolicyReportOnly = "Content-Security-Policy-Report-Only"
	HeaderCrossOriginResourcePolicy       = "Cross-Origin-Resource-Policy"
	HeaderExpectCT                        = "Expect-CT"
	// Deprecated: use HeaderPermissionsPolicy instead
	HeaderFeaturePolicy           = "Feature-Policy"
	HeaderPermissionsPolicy       = "Permissions-Policy"
	HeaderPublicKeyPins           = "Public-Key-Pins"
	HeaderPublicKeyPinsReportOnly = "Public-Key-Pins-Report-Only"
	HeaderStrictTransportSecurity = "Strict-Transport-Security"
	HeaderUpgradeInsecureRequests = "Upgrade-Insecure-Requests"
	HeaderXContentTypeOptions     = "X-Content-Type-Options"
	HeaderXDownloadOptions        = "X-Download-Options"
	HeaderXFrameOptions           = "X-Frame-Options"
	HeaderXPoweredBy              = "X-Powered-By"
	HeaderXXSSProtection          = "X-XSS-Protection"
	HeaderLastEventID             = "Last-Event-ID"
	HeaderNEL                     = "NEL"
	HeaderPingFrom                = "Ping-From"
	HeaderPingTo                  = "Ping-To"
	HeaderReportTo                = "Report-To"
	HeaderTE                      = "TE"
	HeaderTrailer                 = "Trailer"
	HeaderTransferEncoding        = "Transfer-Encoding"
	HeaderSecWebSocketAccept      = "Sec-WebSocket-Accept"
	HeaderSecWebSocketExtensions  = "Sec-WebSocket-Extensions"
	HeaderSecWebSocketKey         = "Sec-WebSocket-Key"
	HeaderSecWebSocketProtocol    = "Sec-WebSocket-Protocol"
	HeaderSecWebSocketVersion     = "Sec-WebSocket-Version"
	HeaderAcceptPatch             = "Accept-Patch"
	HeaderAcceptPushPolicy        = "Accept-Push-Policy"
	HeaderAcceptSignature         = "Accept-Signature"
	HeaderAltSvc                  = "Alt-Svc"
	HeaderDate                    = "Date"
	HeaderIndex                   = "Index"
	HeaderLargeAllocation         = "Large-Allocation"
	HeaderLink                    = "Link"
	HeaderPushPolicy              = "Push-Policy"
	HeaderRetryAfter              = "Retry-After"
	HeaderServerTiming            = "Server-Timing"
	HeaderSignature               = "Signature"
	HeaderSignedHeaders           = "Signed-Headers"
	HeaderSourceMap               = "SourceMap"
	HeaderUpgrade                 = "Upgrade"
	HeaderXDNSPrefetchControl     = "X-DNS-Prefetch-Control"
	HeaderXPingback               = "X-Pingback"
	HeaderXRequestID              = "X-Request-ID"
	HeaderXRequestedWith          = "X-Requested-With"
	HeaderXRobotsTag              = "X-Robots-Tag"
	HeaderXUACompatible           = "X-UA-Compatible"
)

// Network types that are commonly used
const (
	NetworkTCP  = "tcp"
	NetworkTCP4 = "tcp4"
	NetworkTCP6 = "tcp6"
)

// Compression types
const (
	StrGzip    = "gzip"
	StrBr      = "br"
	StrDeflate = "deflate"
	StrBrotli  = "brotli"
)

// Cookie SameSite
// https://datatracker.ietf.org/doc/html/draft-ietf-httpbis-rfc6265bis-03#section-4.1.2.7
const (
	CookieSameSiteDisabled   = "disabled" // not in RFC, just control "SameSite" attribute will not be set.
	CookieSameSiteLaxMode    = "lax"
	CookieSameSiteStrictMode = "strict"
	CookieSameSiteNoneMode   = "none"
)

// Route Constraints
const (
	ConstraintInt             = "int"
	ConstraintBool            = "bool"
	ConstraintFloat           = "float"
	ConstraintAlpha           = "alpha"
	ConstraintGuid            = "guid"
	ConstraintMinLen          = "minLen"
	ConstraintMaxLen          = "maxLen"
	ConstraintLen             = "len"
	ConstraintBetweenLen      = "betweenLen"
	ConstraintMinLenLower     = "minlen"
	ConstraintMaxLenLower     = "maxlen"
	ConstraintBetweenLenLower = "betweenlen"
	ConstraintMin             = "min"
	ConstraintMax             = "max"
	ConstraintRange           = "range"
	ConstraintDatetime        = "datetime"
	ConstraintRegex           = "regex"
)
