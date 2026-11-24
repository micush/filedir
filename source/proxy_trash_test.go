package main

import "testing"

// The Files page can look at the copy of the node the Node menu picked, and at its trash: those calls can be relayed, the rest of the
// Files API (everything that changes something) cannot.
func TestProxyAllowsOnlyReadingAndTheTrashOfFiles(t *testing.T) {
	for _, p := range []string{"/api/files/trash?folder=%2F", "/api/files/trash/list?folder=x&when=&path=", "/api/files/trash/empty", "/api/files/trash/restore", "/api/files/trash/delete"} {
		m := "GET"
		if p == "/api/files/trash/empty" || p == "/api/files/trash/restore" || p == "/api/files/trash/delete" {
			m = "POST"
		}
		if err := proxyAllowed(m, p); err != nil {
			t.Errorf("%s %s: %v", m, p, err)
		}
	}
	if err := proxyAllowed("POST", "/api/files/speedtest"); err != nil { // the speed test of the node the menu picked
		t.Errorf("POST /api/files/speedtest: %v", err)
	}
	for _, p := range []string{"/api/files/list?folder=x", "/api/files/search?q=a", "/api/files/size?folder=x", "/api/files/text?folder=x&path=a", "/api/files/status?local=1"} {
		if err := proxyAllowed("GET", p); err != nil {
			t.Errorf("GET %s: %v", p, err)
		}
	}
	for _, p := range []string{"/api/files/delete", "/api/files/trashy", "/api/files/upload"} {
		if proxyAllowed("POST", p) == nil {
			t.Errorf("%s was allowed", p)
		}
	}
}
