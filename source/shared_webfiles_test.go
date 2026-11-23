package main

import "testing"

// The files site's groups are shared by the cluster, and a cluster that has none hashes as it did before they existed.
func TestSharedWebFilesGroups(t *testing.T) {
	dc := newDaemonConfig()
	base := sharedOf(dc)
	if base.WebFiles != nil {
		t.Fatal("no files site, yet the document carries one")
	}
	dc.Web.FilesGroup, dc.Web.FilesReadGroup = "fileusers", "fileviewers"
	sh := sharedOf(dc)
	if sh.WebFiles == nil || sh.hash() == base.hash() {
		t.Fatal("the groups are not in the shared document")
	}
	if err := sh.Validate(); err != nil {
		t.Fatal(err)
	}
	other := newDaemonConfig()
	if changed, err := mergeShared(other, sh, nil); err != nil || !changed {
		t.Fatalf("merge: %v %v", changed, err)
	}
	if other.Web.FilesGroup != "fileusers" || other.Web.FilesReadGroup != "fileviewers" {
		t.Fatalf("a replica did not take the groups: %+v", other.Web)
	}
	// the primary turns the site off: so does the replica
	off := sharedOf(newDaemonConfig())
	if _, err := mergeShared(other, off, nil); err != nil || other.Web.FilesGroup != "" || other.Web.FilesReadGroup != "" {
		t.Fatalf("the site stayed on: %+v %v", other.Web, err)
	}
}

func TestFilesGroupsAreValidated(t *testing.T) {
	w := defaultWeb()
	w.FilesGroup = "users, viewers"
	if err := w.Validate(); err != nil {
		t.Fatal(err)
	}
	w.FilesReadGroup = "bad:name"
	if err := w.Validate(); err == nil {
		t.Fatal("a group name with a colon was accepted")
	}
}

func TestFilesLinkHost(t *testing.T) {
	dc := newDaemonConfig()
	dc.Groups = []GroupConfig{{GroupID: 1, VIP4: "10.0.0.1/24", VIP6: "fd00::1/64"}}
	if h := filesLinkHost(dc); h != "10.0.0.1" {
		t.Fatalf("vip: %q", h)
	}
	dc.Groups[0].VIP4 = ""
	if h := filesLinkHost(dc); h != "[fd00::1]" {
		t.Fatalf("v6 vip: %q", h)
	}
	dc.Groups[0].VIP4, dc.Groups[0].ExtraVIPs = "10.0.0.1/24", []string{"203.0.113.53", "203.0.113.54"}
	if h := filesLinkHost(dc); h != "203.0.113.53" {
		t.Fatalf("anycast comes first: %q", h)
	}
	dc.Groups[0].PausedVIPs = []string{"203.0.113.53"}
	if h := filesLinkHost(dc); h != "203.0.113.54" {
		t.Fatalf("a paused anycast address is skipped: %q", h)
	}
	dc.Groups[0].PausedAll = true
	if h := filesLinkHost(dc); h != "" {
		t.Fatalf("a paused gateway gives none: %q", h)
	}
}

func TestGatewayFQDNs(t *testing.T) {
	dc := newDaemonConfig()
	dc.Groups = []GroupConfig{{GroupID: 1, VIP4: "10.0.0.1/24", FQDN: "files.example.com", AnycastFQDN: "any.example.com", ExtraVIPs: []string{"203.0.113.53"}}}
	if h := filesLinkHost(dc); h != "any.example.com" {
		t.Fatalf("anycast name first: %q", h)
	}
	dc.Groups[0].PausedAll = false
	dc.Groups[0].PausedVIPs = []string{"203.0.113.53"}
	if h := filesLinkHost(dc); h != "files.example.com" {
		t.Fatalf("then the gateway's name: %q", h)
	}
	dc.Groups[0].FQDN = ""
	if h := filesLinkHost(dc); h != "10.0.0.1" {
		t.Fatalf("then the address: %q", h)
	}
	g := dc.Groups[0]
	g.AnycastFQDN = "bad name"
	if g.Validate() == nil {
		t.Fatal("a name with a space was accepted")
	}
	g.AnycastFQDN, g.FQDN = "ok.example.com", "files.example.com"
	sh := sharedOf(&DaemonConfig{Groups: []GroupConfig{g}})
	var back GroupConfig
	applySharedGroup(&back, sh.Groups[0])
	if back.FQDN != g.FQDN || back.AnycastFQDN != g.AnycastFQDN {
		t.Fatalf("names are not shared: %+v", back)
	}
}
