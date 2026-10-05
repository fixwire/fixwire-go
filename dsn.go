package fixwire

import (
	"errors"
	"net/url"
	"strings"
)

// DSN is where the SDK sends and with which key:
// {scheme}://{key}@{host}[:{port}][/{path}].
type DSN struct {
	// Key is the project's publishable key.
	Key string
	// BaseURL is the DSN without the key; the endpoints are relative to it.
	BaseURL string
}

// ErrInvalidDSN is returned for a DSN without a scheme, a host or a key.
var ErrInvalidDSN = errors.New("fixwire: the DSN must look like https://<key>@<host>")

// ParseDSN reads a DSN.
func ParseDSN(s string) (DSN, error) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User == nil || u.User.Username() == "" {
		return DSN{}, ErrInvalidDSN
	}
	return DSN{
		Key:     u.User.Username(),
		BaseURL: u.Scheme + "://" + u.Host + strings.TrimRight(u.Path, "/"),
	}, nil
}

// URL is the address of an endpoint ("/v1/logs", …).
func (d DSN) URL(path string) string { return d.BaseURL + path }
