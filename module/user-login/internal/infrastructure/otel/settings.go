package otel

import (
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

// ResourceSettings задаёт стабильную идентичность места запуска без autodetection.
type ResourceSettings struct {
	NodeName    string
	Environment string
}

// Validate проверяет обязательные значения без пробельных и управляющих символов.
func (s ResourceSettings) Validate() error {
	for _, field := range []struct{ name, value string }{
		{"USER_AUTH_NODE_NAME", s.NodeName}, {"USER_AUTH_ENVIRONMENT", s.Environment},
	} {
		if field.value == "" || strings.IndexFunc(field.value, func(r rune) bool {
			return unicode.IsSpace(r) || unicode.IsControl(r)
		}) >= 0 {
			return fmt.Errorf("%s: требуется непустое значение без пробельных и управляющих символов", field.name)
		}
	}
	return nil
}

// ValidateEndpoint проверяет полный URL получателя без неявного дополнения пути.
func ValidateEndpoint(endpoint string) error {
	invalid := func() error {
		return fmt.Errorf(
			"USER_AUTH_OTLP_METRICS_ENDPOINT: требуется http/https URL с корректным host, портом 1..65535 и точным путём /v1/metrics без userinfo, query, fragment и кодирования пути",
		)
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(endpoint, "#") ||
		u.Path != "/v1/metrics" || u.RawPath != "" || u.Opaque != "" {
		return invalid()
	}
	if !validEndpointHost(u) {
		return invalid()
	}
	if port := u.Port(); port != "" {
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil || n == 0 {
			return invalid()
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return invalid()
	}
	return nil
}

func validEndpointHost(u *url.URL) bool {
	host := u.Hostname()
	if host == "" {
		return false
	}
	if strings.Contains(u.Host, "[") || strings.Contains(host, ":") {
		ip, err := netip.ParseAddr(host)
		return err == nil && ip.Is6() && ip.Zone() == ""
	}
	if !validEndpointDNSName(host) {
		return false
	}
	if strings.IndexFunc(host, func(r rune) bool { return r != '.' && (r < '0' || r > '9') }) < 0 {
		_, err := netip.ParseAddr(host)
		return err == nil
	}
	return true
}

func validEndpointDNSName(host string) bool {
	if len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(strings.TrimSuffix(host, "."), ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			allowed := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-'
			if !allowed {
				return false
			}
		}
	}
	return true
}
