package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func mustTask(t *testing.T, s SchedTask) SchedTask {
	t.Helper()
	if s.Name == "" {
		s.Name = "t"
	}
	if s.Action == "" {
		s.Action = "stats_clear"
	}
	if err := s.validate(); err != nil {
		t.Fatal(err)
	}
	return s
}

func dates(l []time.Time) string {
	var p []string
	for _, x := range l {
		p = append(p, x.Format("01-02 15:04"))
	}
	return strings.Join(p, ",")
}

func TestScheduleOffsetStaysInTheWindow(t *testing.T) {
	base := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	if schedOffset("n1", "a", base, 0) != 0 {
		t.Fatal("no window, but an offset")
	}
	seen := map[time.Duration]bool{}
	for _, n := range []string{"n1", "n2", "n3", "n4", "n5", "n6"} {
		o := schedOffset(n, "a", base, 3600)
		if o < 0 || o > time.Hour {
			t.Fatalf("%s: offset %v is outside the window", n, o)
		}
		if o != schedOffset(n, "a", base, 3600) {
			t.Fatal("the offset changed between calls")
		}
		seen[o] = true
	}
	if len(seen) < 4 {
		t.Fatalf("the nodes were not spread over the window: %v", seen)
	}
}

type schedHarness struct {
	s   *Scheduler
	now time.Time
	mu  sync.Mutex
	ran []string
}

func newHarness(t *testing.T, tasks ...SchedTask) *schedHarness {
	h := &schedHarness{now: time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)}
	h.s = newScheduler(nil, t.TempDir())
	h.s.now = func() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.now }
	h.s.act = func(action, actor string) (string, error) {
		h.mu.Lock()
		h.ran = append(h.ran, action)
		h.mu.Unlock()
		return "done", nil
	}
	h.s.SetTasks(tasks)
	return h
}

func (h *schedHarness) at(tm time.Time) { h.mu.Lock(); h.now = tm; h.mu.Unlock() }
func (h *schedHarness) count() int      { h.mu.Lock(); defer h.mu.Unlock(); return len(h.ran) }

func waitFor(t *testing.T, f func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if f() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out")
}

// useScripts makes a script folder the scheduler may use and puts the given scripts (name: body) in it.
func useScripts(t *testing.T, scripts map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := schedScriptDir
	schedScriptDir = dir
	t.Cleanup(func() { schedScriptDir = old })
	for n, body := range scripts {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestSchedulerShellCommand(t *testing.T) {
	useScripts(t, map[string]string{
		"echo.sh": "echo hello; echo oops >&2",
		"slow.sh": "sleep 30",
		"bad.sh":  "exit 3",
		"ign.sh":  "echo stopped; exit 3",
		"args.sh": `echo "[$1][$2]"`,
	})
	h := newHarness(t)
	ok := mustTask(t, SchedTask{ID: "s1", Name: "echo", Action: "shell", Command: "echo.sh", Freq: "daily"})
	slow := mustTask(t, SchedTask{ID: "s2", Name: "slow", Action: "shell", Command: "slow.sh", TimeoutSec: 1, Freq: "daily"})
	bad := mustTask(t, SchedTask{ID: "s3", Name: "bad", Action: "shell", Command: "bad.sh", Freq: "daily"})
	ign := mustTask(t, SchedTask{ID: "s4", Name: "ign", Action: "shell", Command: "ign.sh", IgnoreExit: true, Freq: "daily"})
	ignSlow := mustTask(t, SchedTask{ID: "s5", Name: "ignslow", Action: "shell", Command: "slow.sh", TimeoutSec: 1, IgnoreExit: true, Freq: "daily"})
	args := mustTask(t, SchedTask{ID: "s6", Name: "args", Action: "shell", Command: `args.sh 'a b' "c; rm -rf /"`, Freq: "daily"})
	h.s.SetTasks([]SchedTask{ok, slow, bad, ign, ignSlow, args})
	idle := func() bool {
		for _, r := range h.s.Status().History {
			if r.Status == "running" {
				return false
			}
		}
		return true
	}
	for _, id := range []string{"s1", "s2", "s3", "s4", "s5", "s6"} { // one at a time
		if err := h.s.RunNow(id, "test"); err != nil {
			t.Fatal(err)
		}
		waitFor(t, idle)
	}
	by := map[string]SchedRun{}
	for _, r := range h.s.Status().History {
		by[r.Task] = r
	}
	if by["s1"].Status != "ok" || !strings.Contains(by["s1"].Output, "hello") || !strings.Contains(by["s1"].Output, "oops") {
		t.Errorf("echo: %+v", by["s1"])
	}
	if by["s2"].Status != "failed" || !strings.Contains(by["s2"].Output, "stopped after") {
		t.Errorf("a command over its limit: %+v", by["s2"])
	}
	if by["s3"].Status != "failed" {
		t.Errorf("exit 3: %+v", by["s3"])
	}
	if by["s4"].Status != "ok" || !strings.Contains(by["s4"].Output, "stopped") || !strings.Contains(by["s4"].Output, "exit status 3, ignored") {
		t.Errorf("exit 3 with the status ignored: %+v", by["s4"])
	}
	if by["s5"].Status != "failed" {
		t.Errorf("a command over its limit fails even when the status is ignored: %+v", by["s5"])
	}
	if by["s6"].Status != "ok" || !strings.Contains(by["s6"].Output, "[a b][c; rm -rf /]") {
		t.Errorf("arguments reach the script as they are written, with no shell: %+v", by["s6"])
	}
}

func TestSchedScriptRules(t *testing.T) {
	dir := useScripts(t, map[string]string{"ok.sh": "true", "open.sh": "true", "plain": "true"})
	os.Chmod(filepath.Join(dir, "open.sh"), 0o777) // anyone may write it
	os.Chmod(filepath.Join(dir, "plain"), 0o644)   // cannot be run
	os.Symlink("/bin/true", filepath.Join(dir, "link.sh"))
	for cmd, want := range map[string]string{
		"ok.sh":       "",
		"ok.sh a b":   "",
		"../x":        "not a script name",
		"/bin/true":   "not a script name",
		"sub/ok.sh":   "not a script name",
		"":            "empty",
		"ok.sh 'open": "quote",
		"nope.sh":     "no script",
		"open.sh":     "not writable by anyone else",
		"plain":       "runnable",
		"link.sh":     "plain file",
	} {
		_, _, err := schedScript(cmd)
		switch {
		case want == "" && err != nil:
			t.Errorf("%q: %v", cmd, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("%q: want an error about %q, got %v", cmd, want, err)
		}
	}
	os.Chmod(dir, 0o777)
	if _, _, err := schedScript("ok.sh"); err == nil || !strings.Contains(err.Error(), "not writable by anyone else") {
		t.Errorf("a folder anyone can write to: %v", err)
	}
	schedScriptDir = filepath.Join(dir, "missing")
	if _, _, err := schedScript("ok.sh"); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("no folder: %v", err)
	}
	bad := SchedTask{ID: "x", Name: "n", Action: "shell", Command: "a/b", Freq: "daily"}
	if err := bad.validate(); err == nil {
		t.Error("a task naming a folder was accepted")
	}
}

func TestSchedEnvHasTheUsualPaths(t *testing.T) {
	env := schedEnv([]string{"PATH=/opt/x/bin", "LANG=C"}, "job")
	m := map[string]string{}
	for _, e := range env {
		if i := strings.IndexByte(e, '='); i > 0 {
			m[e[:i]] = e[i+1:]
		}
	}
	for _, p := range []string{"/opt/x/bin", "/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"} {
		if !strings.Contains(":"+m["PATH"]+":", ":"+p+":") {
			t.Errorf("PATH %q lacks %s", m["PATH"], p)
		}
	}
	if m["HOME"] != "/root" || m["LANG"] != "C" {
		t.Errorf("env %v", m)
	}
	if got := schedEnv(nil, "job"); len(got) < 2 {
		t.Errorf("empty base: %v", got)
	}
}

// With auto-update on, the online update task is for one node of a cluster; alone, or with auto-update off, it is not asked.
func TestOnlineUpdateNeedsOneNode(t *testing.T) {
	every := SchedTask{Action: "update_online"}
	one := SchedTask{Action: "update_online", Nodes: []string{"a"}}
	two := SchedTask{Action: "update_online", Nodes: []string{"a", "b"}}
	other := SchedTask{Action: "stats_clear"}
	for _, c := range []struct {
		t       SchedTask
		auto    bool
		cluster bool
		want    bool
	}{
		{every, true, true, true}, {two, true, true, true}, {one, true, true, false},
		{every, false, true, false}, {every, true, false, false}, {other, true, true, false},
	} {
		if got := onlineUpdateNeedsOneNode(c.t, c.auto, c.cluster); got != c.want {
			t.Errorf("%+v auto=%v cluster=%v: got %v", c.t, c.auto, c.cluster, got)
		}
	}
	mg := newPowerMgmt(t)
	if st := mg.sched.Status(); st.AutoUpdate != mg.upd.Intent().AutoAll {
		t.Errorf("status says auto-update %v, the intent %v", st.AutoUpdate, mg.upd.Intent().AutoAll)
	}
}
