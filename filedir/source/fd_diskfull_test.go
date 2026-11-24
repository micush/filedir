package main

import (
	"testing"
	"time"
)

// While a "Disk nearly full" alert is open, the trash of the folders on that disk is emptied; with no alert it is left alone.
func TestDiskFullAlertEmptiesTrash(t *testing.T) {
	n := newFdTestNode(t, "a")
	f := fdSvc.get("docs")
	trash := func(name string) {
		t.Helper()
		n.write(t, name, make([]byte, 4096))
		if _, err := f.f.Scan(); err != nil {
			t.Fatal(err)
		}
		if err := f.f.Delete(name); err != nil {
			t.Fatal(err)
		}
		if _, c := f.f.TrashSize(); c == 0 {
			t.Fatal("nothing in the trash")
		}
	}
	forMin := 1
	g := newAlertRig(t, "disk_full", AlertRule{Threshold: 90, ForMin: &forMin})
	g.e.sample = func(time.Time) []alertSample {
		return []alertSample{{Rule: "disk_full", Subject: n.root, Bad: g.bad, Value: 95, Message: "full"}}
	}
	g.e.onDiskFull = func(m string) { fdSvc.emptyTrashOn(m) }
	trash("one.bin")
	g.step(false, 3) // no alert: the trash stays
	if _, c := f.f.TrashSize(); c == 0 {
		t.Fatal("the trash was emptied with no alert")
	}
	g.step(true, 6) // the alert opens
	if g.open() != 1 {
		t.Fatal("the alert did not open")
	}
	if _, c := f.f.TrashSize(); c != 0 {
		t.Fatalf("the trash was not emptied: %d files", c)
	}
	trash("two.bin") // while the alert stays open, what is deleted next is emptied too
	g.step(true, 1)
	if _, c := f.f.TrashSize(); c != 0 {
		t.Fatalf("the trash was not emptied again: %d files", c)
	}
	// a disk that holds no folder of ours is left alone
	trash("three.bin")
	g.e.onDiskFull = func(string) { fdSvc.emptyTrashOn("/proc") }
	g.step(true, 1)
	if _, c := f.f.TrashSize(); c == 0 {
		t.Fatal("the trash of a folder on another disk was emptied")
	}
}
