package caddy

import (
	"strings"
	"testing"
)

func TestRenderSiteBlock(t *testing.T) {
	domain := "git.example.com"
	upstreamIP := "10.0.100.12"
	upstreamPort := 3000
	tls := "dns digitalocean"
	extra := []string{"header Strict-Transport-Security max-age=31536000"}

	block := RenderSiteBlock(domain, upstreamIP, upstreamPort, tls, extra)

	if !strings.Contains(block, "git.example.com {") {
		t.Errorf("missing site header: %s", block)
	}
	if !strings.Contains(block, "reverse_proxy 10.0.100.12:3000") {
		t.Errorf("missing reverse_proxy directive: %s", block)
	}
	if !strings.Contains(block, "tls dns digitalocean") {
		t.Errorf("missing tls directive: %s", block)
	}
	if !strings.Contains(block, "header Strict-Transport-Security") {
		t.Errorf("missing extra directive: %s", block)
	}
}
