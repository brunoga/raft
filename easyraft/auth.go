package easyraft

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

var (
	// ErrUnauthorized reports that a request carried no usable credential.
	// An [WithHTTPAuth] hook that wraps or returns it makes the HTTP layer
	// answer 401 Unauthorized.
	ErrUnauthorized = errors.New("easyraft: unauthorized")

	// ErrForbidden reports that a request carried a valid credential that is
	// not allowed to perform the operation. An [WithHTTPAuth] hook that wraps
	// or returns it makes the HTTP layer answer 403 Forbidden.
	ErrForbidden = errors.New("easyraft: forbidden")

	// ErrReservedCollection is returned when a request names a collection in
	// the reserved "__" namespace. Those collections hold easyraft's own
	// cluster metadata — including the HTTP addresses used for leader
	// redirects — and are not reachable over HTTP at all.
	ErrReservedCollection = errors.New("easyraft: collection name is reserved for internal use")
)

// reservedPrefix marks collection names easyraft keeps for itself.
const reservedPrefix = "__"

// isReservedCollection reports whether name belongs to easyraft's internal
// namespace.
func isReservedCollection(name string) bool {
	return strings.HasPrefix(name, reservedPrefix)
}

// BearerTokenAuth returns an authorization hook for [WithHTTPAuth] that
// requires an "Authorization: Bearer <token>" header matching token. The
// comparison is constant-time, so it does not leak the token through timing.
//
// An empty token yields a hook that rejects every request: an accidentally
// empty configuration must fail closed rather than open.
func BearerTokenAuth(token string) func(*http.Request) error {
	want := []byte(token)
	return func(r *http.Request) error {
		if len(want) == 0 {
			return fmt.Errorf("%w: no bearer token is configured", ErrForbidden)
		}
		header := r.Header.Get("Authorization")
		scheme, value, found := strings.Cut(header, " ")
		if !found || !strings.EqualFold(scheme, "Bearer") {
			return fmt.Errorf("%w: expected an Authorization: Bearer header", ErrUnauthorized)
		}
		if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(value)), want) != 1 {
			return fmt.Errorf("%w: bearer token does not match", ErrUnauthorized)
		}
		return nil
	}
}

// ClientCertAuth returns an authorization hook for [WithHTTPAuth] that accepts
// a request only when it arrived over TLS with a verified client certificate
// whose Common Name is one of allowedCNs. Passing no names accepts any
// certificate the TLS layer has already verified.
//
// It relies on the TLS handshake for verification, so the server's tls.Config
// must set ClientAuth to tls.RequireAndVerifyClientCert and populate ClientCAs;
// see [WithHTTPTLS].
func ClientCertAuth(allowedCNs ...string) func(*http.Request) error {
	allowed := make(map[string]struct{}, len(allowedCNs))
	for _, cn := range allowedCNs {
		allowed[cn] = struct{}{}
	}
	return func(r *http.Request) error {
		if r.TLS == nil {
			return fmt.Errorf("%w: request did not arrive over TLS", ErrUnauthorized)
		}
		if len(r.TLS.VerifiedChains) == 0 || len(r.TLS.VerifiedChains[0]) == 0 {
			return fmt.Errorf("%w: no verified client certificate", ErrUnauthorized)
		}
		if len(allowed) == 0 {
			return nil
		}
		cn := r.TLS.VerifiedChains[0][0].Subject.CommonName
		if _, ok := allowed[cn]; !ok {
			return fmt.Errorf("%w: client certificate %q is not allowed", ErrForbidden, cn)
		}
		return nil
	}
}

// authStatus maps an authorization error to the HTTP status that reports it.
// Anything that is not explicitly an authentication failure is treated as a
// policy denial.
func authStatus(err error) int {
	if errors.Is(err, ErrUnauthorized) {
		return http.StatusUnauthorized
	}
	return http.StatusForbidden
}
