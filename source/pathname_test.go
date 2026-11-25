package main

import "testing"

func TestPathNameOK(t *testing.T) {
	for _, s := range []string{"eth0", "eth0.100", "br-lan", "enp0s3", "filedir1.2", "wlan0:1", "svc", "filedir", "su_do"} {
		if !pathNameOK(s) {
			t.Errorf("%q should be fine", s)
		}
	}
	for _, s := range []string{"", ".", "..", "../x", "a/b", "/etc", "a b", "x\x00", "é", string(make([]byte, 65))} {
		if pathNameOK(s) {
			t.Errorf("%q should be refused", s)
		}
	}
}

func TestNamesThatReachOutsideAreRefused(t *testing.T) {
	if _, err := fixSSSD("../../etc/x"); err == nil {
		t.Error("fixSSSD took a service name with a path in it")
	}
	changed, failed := relaxReversePath("../..", "eth0")
	if len(changed) != 0 || len(failed) == 0 {
		t.Errorf("relaxReversePath took a bad name: changed %v failed %v", changed, failed)
	}
}
