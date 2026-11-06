package centraltest

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// Taking muster down must interrupt its clients, including streams whose
// lifetime is controlled by the caller rather than a tool-call deadline.
func TestCloseActiveSubscription(t *testing.T) {
	for _, tls := range []bool{false, true} {
		name := "HTTP"
		if tls {
			name = "HTTP2"
		}
		t.Run(name, func(t *testing.T) {
			var m *Muster
			var tr *http.Transport
			if tls {
				m, tr = NewStalling(t, "http://backend.invalid/mcp", "beekeeper", "tools/call")
			} else {
				m = New(t, "http://backend.invalid/mcp", "beekeeper")
				tr = http.DefaultTransport.(*http.Transport).Clone()
			}
			defer tr.CloseIdleConnections()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"subscriptions/listen","params":{"_meta":{%q:%q,%q:{}},"notifications":{"resourceSubscriptions":["beekeeper://feed"]}}}`, mcp.MetaKeyProtocolVersion, mcp.LATEST_PROTOCOL_VERSION, mcp.MetaKeyClientCapabilities)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.URL, strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("MCP-Protocol-Version", mcp.LATEST_PROTOCOL_VERSION)
			req.Header.Set("Mcp-Method", "subscriptions/listen")
			res, err := (&http.Client{Transport: tr}).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = res.Body.Close() }()
			if res.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(res.Body)
				t.Fatalf("subscription HTTP status = %d: %s", res.StatusCode, body)
			}
			if tls && res.ProtoMajor != 2 {
				t.Fatalf("subscription protocol = %s, want HTTP/2", res.Proto)
			}
			reader := bufio.NewReader(res.Body)
			if _, err := reader.ReadString('\n'); err != nil {
				t.Fatal(err)
			}
			line, err := reader.ReadString('\n')
			if err != nil || !strings.Contains(line, "subscriptions/acknowledged") {
				t.Fatalf("subscription acknowledgement = %q, %v", line, err)
			}
			closed := make(chan struct{})
			go func() {
				m.Close()
				close(closed)
			}()
			select {
			case <-closed:
			case <-time.After(time.Second):
				// Rescue the old implementation so the regression reports a
				// failure without hanging the rest of the test process.
				cancel()
				<-closed
				t.Fatal("Close waited for the subscriber to cancel")
			}
			_, _ = io.Copy(io.Discard, reader)
			m.Close() // explicit shutdown and t.Cleanup may both call Close.
		})
	}
}
