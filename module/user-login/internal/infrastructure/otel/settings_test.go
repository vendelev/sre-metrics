package otel_test

import (
	"strings"
	"testing"

	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/otel"
)

func TestValidateEndpointHostBoundaries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		host  string
		valid bool
	}{
		{name: "DNS с завершающей точкой", host: "receiver.example.", valid: true},
		{name: "метка из 63 символов", host: strings.Repeat("a", 63), valid: true},
		{name: "метка из 64 символов", host: strings.Repeat("a", 64)},
		{name: "имя из 253 символов", host: strings.Repeat(strings.Repeat("a", 63)+".", 3) + strings.Repeat("b", 61), valid: true},
		{name: "имя из 254 символов", host: strings.Repeat(strings.Repeat("a", 63)+".", 3) + strings.Repeat("b", 62)},
		{name: "пустая метка", host: "receiver..example"},
		{name: "дефис в конце метки", host: "receiver-.example"},
		{name: "пустое имя с портом", host: ":4318"},
		{name: "IPv6 с зоной", host: "[fe80::1%25eth0]:4318"},
		{name: "IPv4 в IPv6", host: "[::ffff:127.0.0.1]:4318", valid: true},
		{name: "числовое имя не IPv4", host: "123"},
		{name: "IPv4 с завершающей точкой", host: "127.0.0.1."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := otel.ValidateEndpoint("http://" + tc.host + "/v1/metrics"); (err == nil) != tc.valid {
				t.Fatalf("неверный результат проверки хоста %q: %v", tc.host, err)
			}
		})
	}
}
