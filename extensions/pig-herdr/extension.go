package pig_herdr

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

const source = "pig-herdr"
const reportTimeout = 400 * time.Millisecond

// Reports are deliberately bounded and awaited. No request-scoped SDK context
// escapes its handler, and no background goroutine survives an extension reload.
type reporter struct {
	mu        sync.Mutex
	bin, pane string
	seq       int64
	state     string
	prompts   int
	sessionID string
	base      []string
	run       func(string, []string) error
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
		if json.Unmarshal([]byte(raw), &base) != nil || !validResume(base) {
			base = nil
		}
	}
	return &reporter{bin: bin, pane: pane, state: "idle", base: base, run: runCLI}
}

func runCLI(bin string, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = 50 * time.Millisecond
	return cmd.Run() // Never inherit terminal IO or log session data.
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

func (r *reporter) args(verb string) []string {
	now := time.Now().UnixNano()
	if now <= r.seq {
		now = r.seq + 1
	}
	r.seq = now
	return []string{"pane", verb, r.pane, "--source", source, "--agent", "pig", "--seq", strconv.FormatInt(now, 10)}
}

func (r *reporter) release() {
	_ = r.run(r.bin, r.args("release-agent"))
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

func (r *reporter) handle(event string, ctx sdk.Context, data map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if event == sdk.EventSessionShutdown {
		// Host shutdown notifications are not lifecycle handlers. Reload must not
		// release a replacement reporter; also honor a reload reason if supplied.
		if data["reason"] != "reload" {
			r.release()
		}
		return
	}
	switch event {
	case sdk.EventSessionStart:
		if value, err := ctx.GetFlag("herdr-resume-args"); err == nil {
			if raw, ok := value.(string); ok && raw != "" {
				var base []string
				if json.Unmarshal([]byte(raw), &base) == nil && validResume(base) {
					r.base = base
				} else {
					r.base = nil
				}
			}
		}
		r.prompts = 0
		idle, err := ctx.IsIdle()
		if err == nil && !idle {
			r.state = "working"
		} else {
			r.state = "idle"
		}
	case sdk.EventAgentStart:
		r.state = "working"
	case sdk.EventAgentEnd, sdk.EventAgentSettled:
		r.state = "idle"
	case sdk.EventUIPromptStart:
		r.prompts++
	case sdk.EventUIPromptEnd:
		if r.prompts > 0 {
			r.prompts--
		}
	}
	id, err := ctx.GetSessionID()
	if err != nil {
		return
	}
	// Clear a previous session's restore command when switching to an unsaved
	// or in-memory session. Reacquire immediately with the new state.
	if r.sessionID != "" && id != r.sessionID {
		r.release()
	}
	r.sessionID = id
	state := r.state
	if r.prompts > 0 {
		state = "blocked"
	}
	args := append(r.args("report-agent"), "--state", state)
	if state == "blocked" {
		args = append(args, "--message", "Pig is waiting for user input")
	}
	file, err := ctx.GetSessionFile()
	if err == nil && file != nil && *file != "" {
		// Pig assigns IDs before saving. Never promise restoration of a session
		// that has not yet been written, or of a --no-session conversation.
		if info, statErr := os.Stat(*file); statErr == nil && !info.IsDir() {
			thinking, _ := ctx.GetThinkingLevel()
			if thinking == "none" {
				thinking = "off"
			}
			argv := resumeArgs(r.base, *file, ctx.ModelQualified(), thinking)
			if argv != nil {
				args = append(args, "--agent-session-id", id, "--agent-session-path", *file, "--")
				args = append(args, argv...)
			}
		}
	}
	_ = r.run(r.bin, args)
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
		sdk.EventAgentStart, sdk.EventAgentEnd, sdk.EventAgentSettled,
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
		ctx.Notify(fmt.Sprintf("Herdr reporting active in pane %s", r.pane), "info")
		return nil
	})
	return e
}
