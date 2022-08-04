package middleware

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/owncloud/ocis/v2/services/proxy/pkg/webdav"
)

var (
	// SupportedAuthStrategies stores configured challenges.
	SupportedAuthStrategies []string

	// ProxyWwwAuthenticate is a list of endpoints that do not rely on reva underlying authentication, such as ocs.
	// services that fallback to reva authentication are declared in the "frontend" command on oCIS. It is a list of
	// regexp.Regexp which are safe to use concurrently.
	ProxyWwwAuthenticate = []regexp.Regexp{*regexp.MustCompile("/ocs/v[12].php/cloud/")}

	_publicPaths = []string{
		"/dav/public-files/",
		"/remote.php/dav/public-files/",
		"/remote.php/ocs/apps/files_sharing/api/v1/tokeninfo/unprotected",
		"/ocs/v1.php/cloud/capabilities",
		"/data",
	}
)

const (
	// WwwAuthenticate captures the Www-Authenticate header string.
	WwwAuthenticate = "Www-Authenticate"
)

// Authenticator is the common interface implemented by all request authenticators.
// The Authenticator may augment the request with user info or anything related to the
// authentication and return the augmented request.
type Authenticator interface {
	Authenticate(*http.Request) (*http.Request, bool)
}

// Authentication is a higher order authentication middleware.
func Authentication(auths []Authenticator, opts ...Option) func(next http.Handler) http.Handler {
	options := newOptions(opts...)
	configureSupportedChallenges(options)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isOIDCTokenAuth(r) ||
				r.URL.Path == "/" ||
				strings.HasPrefix(r.URL.Path, "/.well-known") ||
				r.URL.Path == "/login" ||
				strings.HasPrefix(r.URL.Path, "/js") ||
				strings.HasPrefix(r.URL.Path, "/themes") ||
				strings.HasPrefix(r.URL.Path, "/signin") ||
				strings.HasPrefix(r.URL.Path, "/konnect") ||
				r.URL.Path == "/config.json" ||
				r.URL.Path == "/oidc-callback.html" ||
				r.URL.Path == "/oidc-callback" ||
				r.URL.Path == "/settings.js" {
				// The authentication for this request is handled by the IdP.
				next.ServeHTTP(w, r)
				return
			}

			for _, a := range auths {
				if req, ok := a.Authenticate(r); ok {
					next.ServeHTTP(w, req)
					return
				}
			}
			if !isPublicPath(r.URL.Path) {
				for _, s := range SupportedAuthStrategies {
					userAgentAuthenticateLockIn(w, r, options.CredentialsByUserAgent, s)
				}
			}
			w.WriteHeader(http.StatusUnauthorized)
			// if the request is a PROPFIND return a WebDAV error code.
			// TODO: The proxy has to be smart enough to detect when a request is directed towards a webdav server
			// and react accordingly.
			if webdav.IsWebdavRequest(r) {
				b, err := webdav.Marshal(webdav.Exception{
					Code:    webdav.SabredavPermissionDenied,
					Message: "Authentication error",
				})

				webdav.HandleWebdavError(w, b, err)
			}
		})
	}
}

// The token auth endpoint uses basic auth for clients, see https://openid.net/specs/openid-connect-basic-1_0.html#TokenRequest
// > The Client MUST authenticate to the Token Endpoint using the HTTP Basic method, as described in 2.3.1 of OAuth 2.0.
func isOIDCTokenAuth(req *http.Request) bool {
	return req.URL.Path == "/konnect/v1/token"
}

func isPublicPath(p string) bool {
	for _, pp := range _publicPaths {
		if strings.HasPrefix(p, pp) {
			return true
		}
	}
	return false
}

// configureSupportedChallenges adds known authentication challenges to the current session.
func configureSupportedChallenges(options Options) {
	if options.OIDCIss != "" {
		SupportedAuthStrategies = append(SupportedAuthStrategies, "bearer")
	}

	if options.EnableBasicAuth {
		SupportedAuthStrategies = append(SupportedAuthStrategies, "basic")
	}
}

func writeSupportedAuthenticateHeader(w http.ResponseWriter, r *http.Request) {
	for _, s := range SupportedAuthStrategies {
		w.Header().Add(WwwAuthenticate, fmt.Sprintf("%v realm=\"%s\", charset=\"UTF-8\"", strings.Title(s), r.Host))
	}
}

func removeSuperfluousAuthenticate(w http.ResponseWriter) {
	w.Header().Del(WwwAuthenticate)
}

// userAgentLocker aids in dependency injection for helper methods. The set of fields is arbitrary and the only relation
// they share is to fulfill their duty and lock a User-Agent to its correct challenge if configured.
type userAgentLocker struct {
	w        http.ResponseWriter
	r        *http.Request
	locks    map[string]string // locks represents a reva user-agent:challenge mapping.
	fallback string
}

// userAgentAuthenticateLockIn sets Www-Authenticate according to configured user agents. This is useful for the case of
// legacy clients that do not support protocols like OIDC or OAuth and want to lock a given user agent to a challenge
// such as basic. For more context check https://github.com/cs3org/reva/pull/1350
func userAgentAuthenticateLockIn(w http.ResponseWriter, r *http.Request, locks map[string]string, fallback string) {
	u := userAgentLocker{
		w:        w,
		r:        r,
		locks:    locks,
		fallback: fallback,
	}

	for _, r := range ProxyWwwAuthenticate {
		evalRequestURI(u, r)
	}
}

func evalRequestURI(l userAgentLocker, r regexp.Regexp) {
	if !r.MatchString(l.r.RequestURI) {
		return
	}
	for k, v := range l.locks {
		if strings.Contains(k, l.r.UserAgent()) {
			removeSuperfluousAuthenticate(l.w)
			l.w.Header().Add(WwwAuthenticate, fmt.Sprintf("%v realm=\"%s\", charset=\"UTF-8\"", strings.Title(v), l.r.Host))
			return
		}
	}
	l.w.Header().Add(WwwAuthenticate, fmt.Sprintf("%v realm=\"%s\", charset=\"UTF-8\"", strings.Title(l.fallback), l.r.Host))
}

// AuthenticationOld is a higher order authentication middleware.
func AuthenticationOld(opts ...Option) func(next http.Handler) http.Handler {
	options := newOptions(opts...)

	configureSupportedChallenges(options)
	oidc := newOIDCAuth(options)
	// basic := newBasicAuth(options)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if options.OIDCIss != "" && options.EnableBasicAuth {
				//oidc(basic(next)).ServeHTTP(w, r)
				oidc(next).ServeHTTP(w, r)
			}

			if options.OIDCIss != "" && !options.EnableBasicAuth {
				oidc(next).ServeHTTP(w, r)
			}

			// if options.OIDCIss == "" && options.EnableBasicAuth {
			// 	basic(next).ServeHTTP(w, r)
			// }
		})
	}
}

// newOIDCAuth returns a configured oidc middleware
func newOIDCAuth(options Options) func(http.Handler) http.Handler {
	return OIDCAuth(
		Logger(options.Logger),
		OIDCProviderFunc(options.OIDCProviderFunc),
		HTTPClient(options.HTTPClient),
		OIDCIss(options.OIDCIss),
		TokenCacheSize(options.UserinfoCacheSize),
		TokenCacheTTL(options.UserinfoCacheTTL),
		CredentialsByUserAgent(options.CredentialsByUserAgent),
		AccessTokenVerifyMethod(options.AccessTokenVerifyMethod),
		JWKSOptions(options.JWKS),
	)
}

// // newBasicAuth returns a configured basic middleware
// func newBasicAuth(options Options) func(http.Handler) http.Handler {
// 	return BasicAuth(
// 		UserProvider(options.UserProvider),
// 		Logger(options.Logger),
// 		EnableBasicAuth(options.EnableBasicAuth),
// 		OIDCIss(options.OIDCIss),
// 		UserOIDCClaim(options.UserOIDCClaim),
// 		UserCS3Claim(options.UserCS3Claim),
// 		CredentialsByUserAgent(options.CredentialsByUserAgent),
// 	)
// }
