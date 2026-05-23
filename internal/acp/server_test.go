package acp

import (
	"net/http"
	"testing"
	"time"
)

func TestHTTPServerWriteTimeoutCoversOpenClawAgentWait(t *testing.T) {
	server := newHTTPServer("127.0.0.1:0", http.NewServeMux())

	if server.ReadTimeout != 30*time.Second {
		t.Fatalf("expected fixed read timeout, got %s", server.ReadTimeout)
	}
	if server.IdleTimeout != 2*time.Minute {
		t.Fatalf("expected fixed idle timeout, got %s", server.IdleTimeout)
	}
	if server.WriteTimeout <= openClawAgentWaitMaxTimeout {
		t.Fatalf(
			"expected write timeout %s to exceed OpenClaw max agent.wait timeout %s",
			server.WriteTimeout,
			openClawAgentWaitMaxTimeout,
		)
	}
	if got, want := server.WriteTimeout-openClawAgentWaitMaxTimeout, openClawAgentWaitHTTPMargin; got != want {
		t.Fatalf("expected one-minute write timeout margin, got %s", got)
	}
}
