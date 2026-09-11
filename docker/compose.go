package docker

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// composeArgs builds the base arguments for a `docker compose -p <name>` CLI
// invocation, adding -f flags from the project's config_files label when
// present. The caller appends the desired subcommand (up, down, etc.) and any
// extra flags after calling this.
func composeArgs(p ComposeProject, extra ...string) []string {
	args := []string{"compose", "-p", p.Name}
	if p.ConfigFiles != "" {
		for _, f := range strings.Fields(p.ConfigFiles) {
			args = append(args, "-f", f)
		}
	}
	return append(args, extra...)
}

// composeRun executes a docker compose CLI operation synchronously and returns
// the first error. Stderr is folded into the error message so the TUI can
// surface the daemon's own explanation (e.g. "no configuration file provided").
func composeRun(args []string) error {
	cmd := exec.CommandContext(context.Background(), "docker", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return fmt.Errorf("%s: %s", err, msg)
		}
		return err
	}
	return nil
}

// ComposeUp runs `docker compose -p <name> up -d`.
func (c *Client) ComposeUp(p ComposeProject) error {
	return composeRun(composeArgs(p, "up", "-d"))
}

// ComposeRestart runs `docker compose -p <name> restart`.
func (c *Client) ComposeRestart(p ComposeProject) error {
	return composeRun(composeArgs(p, "restart"))
}

// ComposeDown runs `docker compose -p <name> down`; when volumes is true the
// -v flag is appended to also remove named volumes declared in the compose file.
func (c *Client) ComposeDown(p ComposeProject, volumes bool) error {
	args := composeArgs(p, "down")
	if volumes {
		args = append(args, "-v")
	}
	return composeRun(args)
}

// ComposeLogsArgs returns the arguments to pass to launchTerminal for a
// streaming `docker compose logs -f` session. The caller constructs
// `docker <args>` from the returned slice.
func (c *Client) ComposeLogsArgs(p ComposeProject) []string {
	return composeArgs(p, "logs", "-f", "--tail=100")
}

// ComposeServiceUp starts a single (currently down) service of the project via
// `docker compose -p <name> up -d <svc>`.
func (c *Client) ComposeServiceUp(p ComposeProject, svc string) error {
	return composeRun(composeArgs(p, "up", "-d", svc))
}

// ComposeServiceRestart restarts a single service.
func (c *Client) ComposeServiceRestart(p ComposeProject, svc string) error {
	return composeRun(composeArgs(p, "restart", svc))
}

// ComposeServiceStop stops a single running service.
func (c *Client) ComposeServiceStop(p ComposeProject, svc string) error {
	return composeRun(composeArgs(p, "stop", svc))
}

// ComposeServiceLogsArgs returns the arguments for a streaming logs session
// scoped to a single service: `docker compose -p <name> logs -f --tail=100 <svc>`.
func (c *Client) ComposeServiceLogsArgs(p ComposeProject, svc string) []string {
	return composeArgs(p, "logs", "-f", "--tail=100", svc)
}

// ComposeServiceExecArgs returns the arguments for an interactive shell in the
// selected service: `docker compose -p <name> exec <svc> sh`.
func (c *Client) ComposeServiceExecArgs(p ComposeProject, svc string) []string {
	return composeArgs(p, "exec", svc, "sh")
}
