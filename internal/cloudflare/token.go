package cloudflare

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ConnectorToken is the payload carried by a Zero Trust connector token: the
// same base64 JSON that `cloudflared tunnel run --token` consumes.
//
// Decoding it is what lets DockFlare talk to the tunnel's configuration
// endpoint without asking the user for an account ID and tunnel ID they
// already handed us inside the token.
type ConnectorToken struct {
	AccountTag string `json:"a"`
	TunnelID   string `json:"t"`
	Secret     string `json:"s"`
}

// String redacts the secret so a stray %v or %+v in a log line cannot leak it.
func (t ConnectorToken) String() string {
	return fmt.Sprintf("ConnectorToken{AccountTag:%s TunnelID:%s Secret:[redacted]}", t.AccountTag, t.TunnelID)
}

// ParseConnectorToken decodes a connector token. Errors never echo the token
// or any part of it.
func ParseConnectorToken(raw string) (*ConnectorToken, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("tunnel token is empty")
	}

	// Cloudflare has shipped tokens in both padded and unpadded base64, and
	// both alphabets appear in the wild — try each.
	var (
		data []byte
		err  error
	)
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		if data, err = enc.DecodeString(raw); err == nil {
			break
		}
	}
	if err != nil {
		return nil, errors.New("tunnel token is not valid base64")
	}

	var t ConnectorToken
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, errors.New("tunnel token payload is not valid JSON")
	}
	if t.AccountTag == "" || t.TunnelID == "" {
		return nil, errors.New("tunnel token is missing the account tag or tunnel ID")
	}
	return &t, nil
}
