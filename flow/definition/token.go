package definition

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
)

// tokenVersion is the layout of a page token. A token is opaque to callers
// and bound to the tenant it was issued for.
const tokenVersion = 1

// PageToken is the token List returns for a page that ends at name. A
// backend outside this package issues it so tokens mean the same everywhere.
func PageToken(tenant, name string) string { return encodeToken(tenant, name) }

// PageAfter returns the name a List page token resumes after, refusing a
// token issued for another tenant with ErrInvalid. An empty token is "".
func PageAfter(token, tenant string) (string, error) { return decodeToken(token, tenant) }

func encodeToken(tenant, name string) string {
	buf := binary.AppendUvarint(nil, tokenVersion)
	buf = appendField(buf, tenant)
	buf = appendField(buf, name)
	return base64.RawURLEncoding.EncodeToString(buf)
}

// decodeToken returns the name a page resumes after. A token issued for
// another tenant is refused, so it cannot be used to probe one.
func decodeToken(token, tenant string) (string, error) {
	if token == "" {
		return "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", fmt.Errorf("%w: page token is not one this source issued", ErrInvalid)
	}
	version, n := binary.Uvarint(raw)
	if n <= 0 || version != tokenVersion {
		return "", fmt.Errorf("%w: page token has an unknown layout", ErrInvalid)
	}
	raw = raw[n:]
	issuedFor, raw, err := readField(raw)
	if err != nil {
		return "", fmt.Errorf("%w: page token tenant: %w", ErrInvalid, err)
	}
	name, raw, err := readField(raw)
	if err != nil {
		return "", fmt.Errorf("%w: page token position: %w", ErrInvalid, err)
	}
	switch {
	case len(raw) != 0:
		return "", fmt.Errorf("%w: page token has trailing bytes", ErrInvalid)
	case issuedFor != tenant:
		return "", fmt.Errorf("%w: page token was issued for another tenant", ErrInvalid)
	case name == "":
		return "", fmt.Errorf("%w: page token names no position", ErrInvalid)
	}
	return name, nil
}

func appendField(buf []byte, s string) []byte {
	buf = binary.AppendUvarint(buf, uint64(len(s)))
	return append(buf, s...)
}

func readField(buf []byte) (string, []byte, error) {
	size, n := binary.Uvarint(buf)
	if n <= 0 {
		return "", nil, fmt.Errorf("length prefix is malformed")
	}
	buf = buf[n:]
	if uint64(len(buf)) < size {
		return "", nil, fmt.Errorf("field claims %d bytes, %d remain", size, len(buf))
	}
	return string(buf[:size]), buf[size:], nil
}
