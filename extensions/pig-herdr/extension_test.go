package pig_herdr

import (
	"os"
	"reflect"
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

func TestSequenceAndRelease(t *testing.T) {
	r := &reporter{bin: "herdr", pane: "w1:p1", seq: time.Now().UnixNano() + 1000000000}
	before := r.seq
	var got []string
	r.run = func(_ string, args []string) error { got = args; return os.ErrNotExist }
	r.release()
	if r.seq <= before {
		t.Fatal("sequence went backwards")
	}
	if got[1] != "release-agent" || got[2] != "w1:p1" {
		t.Fatalf("wrong release: %q", got)
	}
}

func TestCLITimeout(t *testing.T) {
	if os.Getenv("PIG_HERDR_TIMEOUT_HELPER") == "1" {
		time.Sleep(10 * time.Second)
		return
	}
	t.Setenv("PIG_HERDR_TIMEOUT_HELPER", "1")
	start := time.Now()
	if runCLI(os.Args[0], []string{"-test.run=^TestCLITimeout$"}) == nil {
		t.Fatal("expected timeout")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("report was not bounded")
	}
}
