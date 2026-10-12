# pig-herdr

A Go Pig extension that reports Pig's lifecycle state and current session to
[Herdr](https://herdr.dev/docs/add-herdr-support/). No Herdr patch or Pi integration
is required. This is separate from the Herdr agent skill, which teaches an agent
to operate Herdr's panes.

## Install

Validate, then install the local package:

```bash
pig package validate /home/azri/projects/personal/pig-herdr --json
pig install /home/azri/projects/personal/pig-herdr/extensions/pig-herdr --validate-only --json
pig install /home/azri/projects/personal/pig-herdr
```

Start a **new Pig process inside a Herdr pane**:

```bash
pig
```

Herdr should list it as `pig`, with its current state:

```bash
herdr agent list
```

Inside Pig, `/herdr-status` confirms the reporter is active. Outside Herdr the
extension makes no reports and does not register that command. Installation is
not automatic: this project's development and tests do not change user settings.

To uninstall:

```bash
pig remove /home/azri/projects/personal/pig-herdr
```

Use Herdr 0.9.2 or later for custom session restoration; older Herdr still gets
state reports. Pig needs a compatible
Go toolchain to build the extension; Pig stages its version-matched SDK.

## Try without installing

Load the extension explicitly, and include that path in the restore base argv:

```bash
pig -e /home/azri/projects/personal/pig-herdr/extensions/pig-herdr \
  --herdr-resume-args '["pig","-e","/home/azri/projects/personal/pig-herdr/extensions/pig-herdr"]'
```

The flag is a **JSON array**, not shell text. It contains the executable and
options needed to recreate your agent configuration, excluding the session,
model, and thinking options (the extension supplies those).

For a Piglet, supply its selection too:

```bash
pig --piglet review -e /home/azri/projects/personal/pig-herdr/extensions/pig-herdr \
  --herdr-resume-args '["pig","--piglet","review","-e","/home/azri/projects/personal/pig-herdr/extensions/pig-herdr"]'
```

Alternatively set `PIG_HERDR_RESUME_ARGS` to the same JSON before starting Pig.
The CLI flag takes precedence. The extension embeds the base argv in its reported
restore command, so it survives later restarts even if that environment variable
is no longer present.

The default base argv is `["pig"]`, suitable for a normally installed extension
with normal discovery. Launch options are **not automatically reconstructed**:
include needed Piglet, tools, prompts, trust, discovery, and other configuration
options yourself. Custom environment variables such as `PIG_HOME` are not
preserved by this extension, and Herdr did not preserve a workspace-injected
`PIG_HOME` in the restore test. For a custom environment, use a command wrapper
on `PATH` that sets it, and use that wrapper as the base executable.
Custom Piglet binaries can use their plain command name as the
first element, provided they include this extension and support the reported flags.
Do not include secrets: Herdr persists the restore command locally.

## Behavior

- Activates only with `HERDR_ENV=1`, `HERDR_BIN_PATH`, `HERDR_PANE_ID`, and
  `HERDR_SOCKET_PATH` present, and reports only from the interactive TUI. Print,
  JSON and RPC runs inside a Herdr pane stay silent.
- Uses Herdr's owning binary, not whichever `herdr` happens to be on `PATH`.
- Reports `working` on `agent_start`, and `idle` on `agent_settled` once Pig is
  idle, so retries and compaction do not flicker to idle.
- Reports `blocked` while Pig SDK UI prompts are open, with the prompt title as
  the sidebar message, and counts overlapping prompts. Built-in screens that do
  not emit these prompt events (for example the model picker) remain idle.
- Refreshes restoration information on session, model, thinking, and message
  events. The session's exact file path handles non-default session directories.
- Only reports a resume command once the session file exists. Unsaved sessions
  and `--no-session` conversations are tracked but cannot be restored.
- Clears the previous session's restore command when switching session IDs.
- Releases the pane only when Pig quits. Other session shutdowns (`/new`,
  `/resume`, `/reload`) stop this reporter without releasing, so the successor's
  report is never cleared by a late release.
- Uses source `pig-herdr` and increasing timestamp-based sequence numbers.
- Sends reports from a background queue, one at a time and in order, keeping only
  the newest pending state, with a 3 s CLI timeout. Pig never waits on Herdr.
  There are no HTTP calls or transcript uploads.
- If Herdr rejects the restore command (Herdr before 0.9.2, or
  `invalid_resume_argv`), resends the report without it and stops attaching one.

Herdr's command restrictions apply: a plain executable name on `PATH`, no
apostrophes or control characters in arguments, at most 64 arguments and 8 KiB.
Invalid restore configuration disables restore reporting without disabling
lifecycle reporting. A failed Herdr call writes one line to the extension's
stderr, and the same failure is not repeated until a call succeeds again. Check
`herdr agent list` and `/herdr-status` when diagnosing integration issues.

The reporting queue, restore fallback, prompt messages, shutdown handling and
diagnostics are adapted from Pigpen's
[herdr component](https://github.com/MichaelKinsy/pigpen/tree/main/components/herdr)
(MIT, Michael Kinsy); see `THIRD_PARTY_NOTICES`.

## Development

```bash
bash test.sh
pig package validate . --json
pig install ./extensions/pig-herdr --validate-only --json
```

`test.sh` uses a temporary module replacement for Pig's staged SDK, leaving
`go.mod` portable. It runs unit tests with the race detector and `go vet`.
Go's normal automatic toolchain selection may be needed for the module's Go 1.26
language floor.

### Verification performed

Tested with the installed Pig and Herdr 0.9.3 in an isolated named Herdr session,
before the background queue and quit-only release were adopted (unit tests
cover those; the end-to-end run below has not been repeated):

- Package installation and normal extension discovery using an isolated Pig config root.
- Pig discovery and `idle → working → idle` during a short model turn.
- `/reload` retained Pig reporting and session restore information.
- Herdr server stop/restart restored the same named session, model, thinking
  level, and explicit extension path.
- An SDK confirmation dialog produced `blocked`, then `idle` after dismissal.
- `/quit` removed Pig from `herdr agent list`.
- `--no-session` did not produce a restore command.

The isolated test server was stopped after verification. No existing Herdr
workspace or session was modified. Windows, older Herdr releases, nested prompts,
and Piglet Binary restoration have not been exercised end to end.
