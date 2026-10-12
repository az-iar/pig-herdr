package pig_herdr

import (
	"bytes"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEnvironmentGuard(t *testing.T) {
	env := map[string]string{"HERDR_ENV": "1", "HERDR_BIN_PATH": "herdr", "HERDR_PANE_ID": "w1:p1", "HERDR_SOCKET_PATH": "/tmp/socket"}
	get := func(k string) string { return env[k] }
	if newReporter(get) == nil {
		t.Fatal("valid Herdr environment disabled")
	}
	for _, key := range []string{"HERDR_ENV", "HERDR_BIN_PATH", "HERDR_PANE_ID", "HERDR_SOCKET_PATH"} {
		value := env[key]
		delete(env, key)
		if newReporter(get) != nil {
			t.Fatalf("enabled without %s", key)
		}
		env[key] = value
	}
	env["PIG_HERDR_RESUME_ARGS"] = `["pig","--piglet","coding"]`
	if !reflect.DeepEqual(newReporter(get).base, []string{"pig", "--piglet", "coding"}) {
		t.Fatal("resume configuration lost")
	}
	env["PIG_HERDR_RESUME_ARGS"] = `invalid`
	if newReporter(get).base != nil {
		t.Fatal("invalid configuration must disable restore")
	}
}

func TestResumeValidation(t *testing.T) {
	for _, argv := range [][]string{nil, {}, {"/bin/pig"}, {".."}, {"pig", "it's"}, {"pig", "new\nline"}, {"pig", "\x00"}} {
		if validResume(argv) {
			t.Fatalf("accepted unsafe argv: %q", argv)
		}
	}
	many := make([]string, 65)
	many[0] = "pig"
	if validResume(many) {
		t.Fatal("accepted too many arguments")
	}
	if validResume([]string{"pig", string(make([]byte, 8193))}) {
		t.Fatal("accepted oversized argv")
	}
	want := []string{"pig", "-e", "/extensions/pig-herdr", "--herdr-resume-args", `["pig","-e","/extensions/pig-herdr"]`, "--session", "/sessions/a b.jsonl", "--model", "provider/model", "--thinking", "high"}
	got := resumeArgs([]string{"pig", "-e", "/extensions/pig-herdr"}, "/sessions/a b.jsonl", "provider/model", "high")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
	if resumeArgs([]string{"pig"}, "", "", "") != nil {
		t.Fatal("unsaved session resumable")
	}
	if resumeArgs(nil, "/session", "", "") != nil {
		t.Fatal("disabled base resumable")
	}
}

func TestMessages(t *testing.T) {
	if got := messageFrom("  Allow\n\tthis?\uFEFF "); got != "Allow this?" {
		t.Fatalf("got %q", got)
	}
	if got := []rune(messageFrom(strings.Repeat("x", 500))); len(got) != maxMessageLength || got[len(got)-1] != '…' {
		t.Fatalf("not truncated: %d", len(got))
	}
	if !reflect.DeepEqual(messageArgs("--"), []string{"--message=--"}) || !reflect.DeepEqual(messageArgs("-x"), []string{"--message", "-x"}) {
		t.Fatal("wrong message form")
	}
}

// fakeHerdr records calls and answers each with respond.
type fakeHerdr struct {
	mu      sync.Mutex
	calls   [][]string
	respond func(args []string) ([]byte, error)
	gate    chan struct{}
}

func (f *fakeHerdr) run(_ string, args []string) ([]byte, error) {
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	f.calls = append(f.calls, args)
	respond := f.respond
	f.mu.Unlock()
	if respond != nil {
		return respond(args)
	}
	return nil, nil
}

func (f *fakeHerdr) snapshot() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string{}, f.calls...)
}

func newTestReporter(f *fakeHerdr) (*reporter, *bytes.Buffer) {
	var stderr bytes.Buffer
	r := &reporter{bin: "herdr", pane: "w1:p1", base: []string{"pig"}, run: f.run, stderr: &stderr, active: true}
	return r, &stderr
}

func (r *reporter) wait(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	done := r.drained
	sending := r.sending
	r.mu.Unlock()
	if !sending {
		return
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not finish")
	}
}

// waitPicked waits until the drain goroutine has taken the queued report.
func (r *reporter) waitPicked(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r.mu.Lock()
		picked := r.queued == nil
		r.mu.Unlock()
		if picked {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("report was not picked up")
		}
		time.Sleep(time.Millisecond)
	}
}

func (r *reporter) set(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn()
	r.publishLocked(false)
}

func flag(args []string, name string) string {
	for i, arg := range args {
		if arg == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func TestReportsCoalesceInOrder(t *testing.T) {
	f := &fakeHerdr{gate: make(chan struct{})}
	r, _ := newTestReporter(f)
	r.set(func() { r.working = true })
	r.waitPicked(t)
	r.set(func() { r.prompts, r.message = 1, "Allow?" })
	r.set(func() { r.prompts, r.message = 0, "" })
	r.set(func() { r.working = false })
	close(f.gate)
	r.wait(t)
	calls := f.snapshot()
	if len(calls) != 2 {
		t.Fatalf("want in-flight report plus newest, got %q", calls)
	}
	if flag(calls[0], "--state") != "working" || flag(calls[1], "--state") != "idle" {
		t.Fatalf("wrong states: %q", calls)
	}
	if flag(calls[0], "--seq") >= flag(calls[1], "--seq") {
		t.Fatal("sequence not increasing")
	}
}

func TestBlockedMessageAndDuplicates(t *testing.T) {
	f := &fakeHerdr{}
	r, _ := newTestReporter(f)
	r.set(func() { r.prompts, r.message = 1, "Allow edit?" })
	r.wait(t)
	r.set(func() {})
	r.wait(t)
	r.set(func() { r.message = "" })
	r.wait(t)
	calls := f.snapshot()
	if len(calls) != 2 {
		t.Fatalf("unchanged state was re-reported: %q", calls)
	}
	if flag(calls[0], "--message") != "Allow edit?" || flag(calls[1], "--message") != defaultBlocked {
		t.Fatalf("wrong messages: %q", calls)
	}
}

func TestResumeRefusedFallsBack(t *testing.T) {
	refusal := exec.Command("sh", "-c", "exit 2").Run()
	f := &fakeHerdr{respond: func(args []string) ([]byte, error) {
		for _, arg := range args {
			if arg == "--" {
				return []byte("error: unknown option: --\n"), refusal
			}
		}
		return nil, nil
	}}
	r, stderr := newTestReporter(f)
	r.set(func() {
		r.session = identity{id: "s1", file: "/s1.jsonl", resume: []string{"pig", "--session", "/s1.jsonl"}}
	})
	r.wait(t)
	r.set(func() { r.working = true })
	r.wait(t)
	calls := f.snapshot()
	if len(calls) != 3 {
		t.Fatalf("want refused, retried and plain report, got %q", calls)
	}
	for _, call := range calls[1:] {
		if strings.Contains(strings.Join(call, " "), " -- ") {
			t.Fatalf("restore command sent after refusal: %q", call)
		}
	}
	if !r.resumeOff || stderr.Len() != 0 {
		t.Fatalf("resumeOff=%v stderr=%q", r.resumeOff, stderr.String())
	}
}

func TestTimeoutIsNotARefusal(t *testing.T) {
	if resumeRefused([]byte("unknown option: --"), os.ErrDeadlineExceeded) {
		t.Fatal("non-exit failure treated as refusal")
	}
}

func TestSessionSwitchClearsRestore(t *testing.T) {
	f := &fakeHerdr{}
	r, _ := newTestReporter(f)
	r.set(func() {
		r.session = identity{id: "a", file: "/a.jsonl", resume: []string{"pig", "--session", "/a.jsonl"}}
	})
	r.wait(t)
	r.set(func() { r.session = identity{id: "b"} })
	r.wait(t)
	calls := f.snapshot()
	if len(calls) != 3 || calls[1][1] != "release-agent" || calls[2][1] != "report-agent" || flag(calls[2], "--agent-session-id") != "b" {
		t.Fatalf("want report, release, report: %q", calls)
	}
}

func TestReleaseIsLast(t *testing.T) {
	f := &fakeHerdr{gate: make(chan struct{})}
	r, _ := newTestReporter(f)
	r.set(func() { r.working = true })
	r.waitPicked(t)
	r.set(func() { r.working = false })
	done := make(chan struct{})
	go func() { r.release(); close(done) }()
	time.Sleep(20 * time.Millisecond)
	close(f.gate)
	<-done
	r.set(func() { r.working = true })
	r.wait(t)
	calls := f.snapshot()
	if len(calls) != 2 || calls[0][1] != "report-agent" || calls[1][1] != "release-agent" {
		t.Fatalf("want in-flight report then release only: %q", calls)
	}
	if flag(calls[0], "--seq") >= flag(calls[1], "--seq") {
		t.Fatal("release sequence not after report")
	}
}

func TestSilenceDoesNotRelease(t *testing.T) {
	f := &fakeHerdr{}
	r, _ := newTestReporter(f)
	r.silence()
	r.set(func() { r.working = true })
	r.wait(t)
	if calls := f.snapshot(); len(calls) != 0 {
		t.Fatalf("silenced reporter called Herdr: %q", calls)
	}
}

func TestDiagnosticsAreDeduplicated(t *testing.T) {
	fail := true
	f := &fakeHerdr{respond: func([]string) ([]byte, error) {
		if fail {
			return []byte("\n  no such pane  \n"), os.ErrNotExist
		}
		return nil, nil
	}}
	r, stderr := newTestReporter(f)
	for _, working := range []bool{true, false, true} {
		r.set(func() { r.working = working })
		r.wait(t)
	}
	if got := strings.Count(stderr.String(), "\n"); got != 1 || !strings.Contains(stderr.String(), "report-agent failed: file does not exist: no such pane") {
		t.Fatalf("stderr: %q", stderr.String())
	}
	fail = false
	r.set(func() { r.working = false })
	r.wait(t)
	fail = true
	r.set(func() { r.working = true })
	r.wait(t)
	if got := strings.Count(stderr.String(), "\n"); got != 2 {
		t.Fatalf("failure after recovery not reported: %q", stderr.String())
	}
}

func TestCLITimeout(t *testing.T) {
	if os.Getenv("PIG_HERDR_TIMEOUT_HELPER") == "1" {
		time.Sleep(10 * time.Second)
		return
	}
	t.Setenv("PIG_HERDR_TIMEOUT_HELPER", "1")
	start := time.Now()
	if _, err := runCLI(os.Args[0], []string{"-test.run=^TestCLITimeout$"}); err == nil {
		t.Fatal("expected timeout")
	}
	if time.Since(start) > callTimeout+2*time.Second {
		t.Fatal("report was not bounded")
	}
}
