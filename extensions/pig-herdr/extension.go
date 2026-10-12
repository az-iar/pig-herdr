package pig_herdr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

const (
	source = "pig-herdr"
	// callTimeout bounds every Herdr CLI call. Reports run off the handler, so
	// a slow Herdr never stalls Pig.
	callTimeout      = 3 * time.Second
	maxMessageLength = 120
	defaultBlocked   = "Pig is waiting for user input"
)

var (
	seqMu   sync.Mutex
	lastSeq int64
)

// nextSeq is monotonic across every reporter in this process and, seeded from
// the clock, across restarts. Herdr drops reports whose seq is not higher than
// the last one it accepted, so the unit must never shrink.
func nextSeq() int64 {
	seqMu.Lock()
	defer seqMu.Unlock()
	lastSeq = max(lastSeq+1, time.Now().UnixNano())
	return lastSeq
}

type identity struct {
	id, file string
	resume   []string
}

func (i identity) equal(o identity) bool {
	return i.id == o.id && i.file == o.file && strings.Join(i.resume, "\x00") == strings.Join(o.resume, "\x00")
}

type report struct {
	state, message string
	seq            int64
	session        identity
}

// reporter is one Herdr reporting session. Handlers may run concurrently, so
// every field is guarded by mu; host and Herdr calls run outside it. No SDK
// context escapes its handler, and the drain goroutine exits once the queue is
// empty.
type reporter struct {
	bin, pane string
	run       func(bin string, args []string) ([]byte, error)
	stderr    io.Writer

	mu       sync.Mutex
	base     []string
	active   bool
	released bool
	working  bool
	// Prompts currently open: producers can overlap, so count.
	prompts int
	message string
	session identity

	hasLast     bool
	lastState   string
	lastMessage string
	lastSession identity

	// resumeOff is set once Herdr refused a restore command (Herdr before
	// 0.9.2, or invalid_resume_argv): later reports leave it out.
	resumeOff bool
	// sentID is the session of the last report Herdr accepted, so a switch to
	// another session clears the previous restore command.
	sentID         string
	lastDiagnostic string

	// Only the newest state matters: while a call is in flight a newer report
	// replaces the queued one, and calls never overlap, so Herdr sees them in order.
	queued  *report
	sending bool
	drained chan struct{}
}

func newReporter(getenv func(string) string) *reporter {
	if getenv("HERDR_ENV") != "1" {
		return nil
	}
	bin, pane := getenv("HERDR_BIN_PATH"), getenv("HERDR_PANE_ID")
	if bin == "" || pane == "" || getenv("HERDR_SOCKET_PATH") == "" {
		return nil
	}
	base := []string{"pig"}
	if raw := getenv("PIG_HERDR_RESUME_ARGS"); raw != "" {
		base = parseBase(raw)
	}
	return &reporter{bin: bin, pane: pane, base: base, run: runCLI, stderr: os.Stderr}
}

func parseBase(raw string) []string {
	var base []string
	if json.Unmarshal([]byte(raw), &base) != nil || !validResume(base) {
		return nil
	}
	return base
}

func runCLI(bin string, args []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	cmd := newCommand(ctx, bin, args)
	// A grandchild holding the pipe must not keep Wait from returning.
	cmd.WaitDelay = time.Second
	return cmd.CombinedOutput()
}

func validResume(argv []string) bool {
	if len(argv) == 0 || len(argv) > 64 || argv[0] == "" {
		return false
	}
	for _, r := range argv[0] {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-", r)) {
			return false
		}
	}
	if argv[0] == "." || argv[0] == ".." {
		return false
	}
	size := 0
	for _, arg := range argv {
		size += len(arg) + 1
		for _, r := range arg {
			if r == '\'' || unicode.IsControl(r) {
				return false
			}
		}
	}
	return size <= 8192
}

func resumeArgs(base []string, file, model, thinking string) []string {
	if len(base) == 0 || file == "" {
		return nil
	}
	argv := append([]string{}, base...)
	encoded, _ := json.Marshal(base)
	argv = append(argv, "--herdr-resume-args", string(encoded))
	argv = append(argv, "--session", file)
	if model != "" {
		argv = append(argv, "--model", model)
	}
	if thinking != "" {
		argv = append(argv, "--thinking", thinking)
	}
	if !validResume(argv) {
		return nil
	}
	return argv
}

// messageFrom makes one short line for Herdr's sidebar; prompt titles can be long and multi-line.
func messageFrom(value string) string {
	line := strings.Join(strings.FieldsFunc(value, func(r rune) bool { return unicode.IsSpace(r) || r == '\uFEFF' }), " ")
	runes := []rune(line)
	if len(runes) > maxMessageLength {
		return string(runes[:maxMessageLength-1]) + "…"
	}
	return line
}

// messageArgs uses the separate-value form every Herdr parses. Only a message
// that is exactly `--` goes attached, because Herdr splits the restore command
// off at the first `--` argument.
func messageArgs(message string) []string {
	if message == "--" {
		return []string{"--message=" + message}
	}
	return []string{"--message", message}
}

// readIdentity reads the session and restore information from the host. A
// restore command is only offered once the session file exists: Pig assigns
// IDs before saving, and --no-session conversations are never written.
func (r *reporter) readIdentity(ctx sdk.Context) (identity, bool) {
	id, err := ctx.GetSessionID()
	if err != nil {
		return identity{}, false
	}
	out := identity{id: id}
	file, err := ctx.GetSessionFile()
	if err != nil || file == nil || *file == "" {
		return out, true
	}
	if info, statErr := os.Stat(*file); statErr != nil || info.IsDir() {
		return out, true
	}
	out.file = *file
	thinking, _ := ctx.GetThinkingLevel()
	if thinking == "none" {
		thinking = "off"
	}
	r.mu.Lock()
	base := r.base
	r.mu.Unlock()
	out.resume = resumeArgs(base, out.file, ctx.ModelQualified(), thinking)
	return out, true
}

func (r *reporter) handle(event string, ctx sdk.Context, data map[string]any) {
	switch event {
	case sdk.EventSessionStart:
		r.start(ctx)
		return
	case sdk.EventSessionShutdown:
		if !r.isActive() {
			return
		}
		if reason, _ := data["reason"].(string); reason != "quit" {
			// A successor reporter re-reports on its own. Releasing here would
			// race that report, and a late release would clear the pane.
			r.silence()
			return
		}
		r.release()
		return
	case sdk.EventAgentSettled:
		// agent_settled fires once no retry, compaction or queued continuation follows.
		if idle, err := ctx.IsIdle(); err == nil && !idle {
			return
		}
	}
	if !r.isActive() {
		return
	}
	id, ok := r.readIdentity(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	if ok {
		r.session = id
	}
	switch event {
	case sdk.EventAgentStart:
		r.working = true
	case sdk.EventAgentSettled:
		r.working = false
	case sdk.EventUIPromptStart:
		title, _ := data["title"].(string)
		if title == "" {
			title, _ = data["kind"].(string)
		}
		r.prompts++
		r.message = messageFrom(title)
	case sdk.EventUIPromptEnd:
		r.prompts = max(0, r.prompts-1)
		if r.prompts == 0 {
			r.message = ""
		}
	}
	r.publishLocked(false)
}

func (r *reporter) start(ctx sdk.Context) {
	// Herdr shows a pane's terminal, so report only the interactive session
	// and stay silent in RPC, JSON and print runs.
	if ctx.Mode() != "tui" {
		return
	}
	if value, err := ctx.GetFlag("herdr-resume-args"); err == nil {
		if raw, ok := value.(string); ok && raw != "" {
			base := parseBase(raw)
			r.mu.Lock()
			r.base = base
			r.mu.Unlock()
		}
	}
	// A reload can start this reporter in the middle of a turn.
	idle, err := ctx.IsIdle()
	id, ok := r.readIdentity(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active, r.released = true, false
	r.prompts, r.message = 0, ""
	r.working = err == nil && !idle
	if ok {
		r.session = id
	}
	r.publishLocked(true)
}

func (r *reporter) isActive() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active
}

func (r *reporter) desiredLocked() (string, string) {
	if r.prompts > 0 {
		if r.message == "" {
			return "blocked", defaultBlocked
		}
		return "blocked", r.message
	}
	if r.working {
		return "working", ""
	}
	return "idle", ""
}

// publishLocked queues a report when the state or the restore information
// (model, thinking, a newly saved session) changed.
func (r *reporter) publishLocked(force bool) {
	state, message := r.desiredLocked()
	if !force && r.hasLast && state == r.lastState && message == r.lastMessage && r.session.equal(r.lastSession) {
		return
	}
	r.hasLast, r.lastState, r.lastMessage, r.lastSession = true, state, message, r.session
	if r.released {
		// A report after the release would reclaim a pane whose agent has exited.
		return
	}
	r.queued = &report{state: state, message: message, seq: nextSeq(), session: r.session}
	if !r.sending {
		r.sending = true
		r.drained = make(chan struct{})
		go r.drain(r.drained)
	}
}

// silence stops reporting without releasing the pane.
func (r *reporter) silence() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.released = true
	r.queued = nil
}

// drain sends queued reports one at a time until none is left.
func (r *reporter) drain(done chan struct{}) {
	defer close(done)
	for {
		r.mu.Lock()
		next := r.queued
		r.queued = nil
		if next == nil {
			r.sending = false
			r.mu.Unlock()
			return
		}
		switched := r.sentID != "" && next.session.id != r.sentID
		withResume := !r.resumeOff
		r.mu.Unlock()

		if switched {
			// Clear the previous session's restore command; the report below
			// reacquires the pane with the new session.
			r.call(r.releaseArgs(nextSeq()))
		}
		args := r.reportArgs(*next, withResume)
		out, err := r.run(r.bin, args)
		if err != nil && withResume && resumeRefused(out, err) {
			// Herdr did not apply the report. Send it once more without the
			// command and stop attaching one: state and release keep working.
			r.mu.Lock()
			r.resumeOff = true
			r.mu.Unlock()
			args = r.reportArgs(*next, false)
			out, err = r.run(r.bin, args)
		}
		r.result(args, out, err)
		if err == nil {
			r.mu.Lock()
			r.sentID = next.session.id
			r.mu.Unlock()
		}
	}
}

func (r *reporter) reportArgs(rep report, withResume bool) []string {
	args := []string{
		"pane", "report-agent", r.pane,
		"--source", source, "--agent", "pig",
		"--state", rep.state, "--seq", strconv.FormatInt(rep.seq, 10),
	}
	if rep.message != "" {
		args = append(args, messageArgs(rep.message)...)
	}
	if rep.session.id != "" {
		args = append(args, "--agent-session-id", rep.session.id)
	}
	if rep.session.file != "" {
		args = append(args, "--agent-session-path", rep.session.file)
	}
	// The command goes last, after `--`. The state report in the same call is
	// what makes this source hold the pane, which Herdr requires first.
	if withResume && rep.session.resume != nil {
		args = append(args, "--")
		args = append(args, rep.session.resume...)
	}
	return args
}

func (r *reporter) releaseArgs(seq int64) []string {
	return []string{
		"pane", "release-agent", r.pane,
		"--source", source, "--agent", "pig", "--seq", strconv.FormatInt(seq, 10),
	}
}

// release stops new reports, drops the queued one and lets the in-flight call
// finish so the release is the last thing Herdr hears from this pane.
func (r *reporter) release() {
	r.mu.Lock()
	r.released = true
	r.queued = nil
	var inFlight chan struct{}
	if r.sending {
		inFlight = r.drained
	}
	seq := nextSeq()
	r.mu.Unlock()
	if inFlight != nil {
		<-inFlight
	}
	r.call(r.releaseArgs(seq))
}

func (r *reporter) call(args []string) {
	out, err := r.run(r.bin, args)
	r.result(args, out, err)
}

// result writes one line to stderr, which Pig collects as the extension's
// output, when Herdr did not take a call: a Herdr that never hears about a pane
// is otherwise indistinguishable from one that was told. The same failure is
// written once until a call succeeds again.
func (r *reporter) result(args []string, out []byte, err error) {
	r.mu.Lock()
	if err == nil {
		r.lastDiagnostic = ""
		r.mu.Unlock()
		return
	}
	msg := "pig-herdr: pane " + args[1] + " failed: " + err.Error()
	if line := firstLine(out); line != "" {
		msg += ": " + line
	}
	repeat := msg == r.lastDiagnostic
	r.lastDiagnostic = msg
	r.mu.Unlock()
	if !repeat {
		fmt.Fprintln(r.stderr, msg)
	}
}

// firstLine is the first non-empty line of Herdr's output, cut to a sensible length.
func firstLine(out []byte) string {
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			if len(line) > 200 {
				line = line[:200] + "..."
			}
			return line
		}
	}
	return ""
}

// resumeRefused reports whether a report failed because Herdr does not take a
// restore command: Herdr before 0.9.2 does not know the `--` separator
// ("unknown option: --"), and 0.9.2+ answers invalid_resume_argv without
// applying the report. A timeout or any other failure is not a verdict.
func resumeRefused(out []byte, err error) bool {
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return false
	}
	text := strings.ToLower(string(out))
	if strings.Contains(text, "invalid_resume_argv") {
		return true
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasSuffix(strings.TrimSpace(line), "unknown option: --") {
			return true
		}
	}
	return false
}

func Extension() *sdk.Extension {
	e := sdk.New(source)
	e.Flag("herdr-resume-args", sdk.FlagOptions{Type: sdk.FlagString, Description: "JSON base argv used for Herdr session restore"})
	r := newReporter(os.Getenv)
	if r == nil {
		return e
	}
	for _, event := range []string{
		sdk.EventSessionStart, sdk.EventSessionShutdown, sdk.EventSessionInfoChanged,
		sdk.EventAgentStart, sdk.EventAgentSettled,
		sdk.EventUIPromptStart, sdk.EventUIPromptEnd,
		sdk.EventMessageEnd, sdk.EventModelSelect, sdk.EventThinkingLevelSelect,
		sdk.EventSessionTree, sdk.EventSessionCompact,
	} {
		e.OnEvent(event, func(ctx sdk.Context, data map[string]any) (any, error) {
			r.handle(event, ctx, data)
			return nil, nil
		})
	}
	e.Command("herdr-status", "Show whether Pig's Herdr reporter is active.", func(ctx sdk.Context, _ string) error {
		if !r.isActive() {
			ctx.Notify(fmt.Sprintf("Herdr pane %s detected, but reporting is off outside the interactive TUI", r.pane), "info")
			return nil
		}
		r.mu.Lock()
		resumeOff := r.resumeOff
		r.mu.Unlock()
		msg := fmt.Sprintf("Herdr reporting active in pane %s", r.pane)
		if resumeOff {
			msg += " (session restore unsupported by this Herdr)"
		}
		ctx.Notify(msg, "info")
		return nil
	})
	return e
}
