package pluginhost

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestHostAuthEgressIDGroupsDirectAndProxyCredentialsWithoutLeakingURL(t *testing.T) {
	host := &Host{runtimeConfig: &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "socks5://global-user:secret@proxy.example:1080"}}}
	direct := &coreauth.Auth{ProxyURL: "direct"}
	globalProxy := &coreauth.Auth{}
	sameProxy := &coreauth.Auth{ProxyURL: "socks5://global-user:secret@proxy.example:1080"}
	otherProxy := &coreauth.Auth{ProxyURL: "http://other.example:8080"}

	if got := host.hostAuthEgressID(direct); got != "direct" {
		t.Fatalf("direct egress ID = %q", got)
	}
	globalID := host.hostAuthEgressID(globalProxy)
	if globalID == "" || globalID == "direct" || globalID != host.hostAuthEgressID(sameProxy) {
		t.Fatalf("global/same proxy IDs = %q / %q", globalID, host.hostAuthEgressID(sameProxy))
	}
	if globalID == host.hostAuthEgressID(otherProxy) {
		t.Fatal("different proxies share one egress ID")
	}
	for _, secret := range []string{"global-user", "secret", "proxy.example", "socks5"} {
		if strings.Contains(globalID, secret) {
			t.Fatalf("egress ID leaked %q: %q", secret, globalID)
		}
	}
}
