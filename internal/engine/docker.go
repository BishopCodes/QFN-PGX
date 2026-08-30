package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// Docker is the seam every engine operation goes through; tests fake it and
// assert the exact argv that serve.sh would have produced.
type Docker interface {
	// Run executes `docker <args...>` and returns combined output.
	Run(ctx context.Context, args ...string) (string, error)
	// FollowLogs streams container logs to w until ctx is cancelled.
	FollowLogs(ctx context.Context, name string, w io.Writer) error
}

// CLIDocker shells out to the docker CLI (always present on the Spark per the
// upstream requirements; avoids the daemon SDK's dependency weight).
type CLIDocker struct {
	Bin    string // default "docker"
	Env    []string
	Stderr *bytes.Buffer // last command's stderr, best-effort, for error text
}

func (d *CLIDocker) bin() string {
	if d.Bin == "" {
		return "docker"
	}
	return d.Bin
}

func (d *CLIDocker) Run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, d.bin(), args...)
	cmd.Env = d.Env
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

func (d *CLIDocker) FollowLogs(ctx context.Context, name string, w io.Writer) error {
	cmd := exec.CommandContext(ctx, d.bin(), "logs", "-f", "--tail", "200", name)
	cmd.Env = d.Env
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { <-ctx.Done(); _ = cmd.Process.Kill() }()
	_, copyErr := io.Copy(w, stdout)
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return nil // normal end of `logs -f`
	}
	if copyErr != nil {
		return copyErr
	}
	return waitErr
}

// NotFoundError models "no such container/image" so callers can branch without
// string-matching docker's locale-sensitive messages (we still do, lightly).
type NotFoundError struct{ What string }

func (e *NotFoundError) Error() string { return e.What + ": not found" }

func looksNotFound(out string, err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(out + " " + err.Error())
	return strings.Contains(s, "no such") || strings.Contains(s, "not found")
}

// ContainerState is the `docker inspect` view Manager exposes.
type ContainerState struct {
	Name      string
	Status    string // created|running|exited|restarting|paused|dead
	StartedAt time.Time
	FinishedAt time.Time
	ExitCode  int
	Running   bool
}

// Inspect reads the container's state; NotFoundError when absent.
func Inspect(ctx context.Context, d Docker, name string) (ContainerState, error) {
	out, err := d.Run(ctx, "inspect", "-f",
		"{{.State.Status}}|{{.State.StartedAt}}|{{.State.FinishedAt}}|{{.State.ExitCode}}", name)
	if err != nil || looksNotFound(out, err) {
		return ContainerState{}, &NotFoundError{What: "container " + name}
	}
	parts := strings.Split(strings.TrimSpace(out), "|")
	if len(parts) != 4 {
		return ContainerState{}, fmt.Errorf("unexpected docker inspect output %q", out)
	}
	st := ContainerState{Name: name, Status: parts[0], Running: parts[0] == "running"}
	st.StartedAt, _ = time.Parse(time.RFC3339Nano, parts[1])
	st.FinishedAt, _ = time.Parse(time.RFC3339Nano, parts[2])
	_, _ = fmt.Sscanf(parts[3], "%d", &st.ExitCode)
	return st, nil
}

// ImageExists reports whether the engine image is present locally, and its
// first repo digest ("" when it was only built, never pulled/tagged-by-digest).
func ImageExists(ctx context.Context, d Docker, image string) (bool, string, error) {
	out, err := d.Run(ctx, "image", "inspect", "-f", "{{index .RepoDigests 0}}", image)
	if err != nil {
		if looksNotFound(out, err) {
			// Distinguish "image absent" from "image present, no digest":
			_, err2 := d.Run(ctx, "image", "inspect", image)
			if err2 != nil {
				return false, "", nil
			}
			return true, "", nil
		}
		return false, "", err
	}
	return true, strings.TrimSpace(out), nil
}

// Stop stops a running container with a grace window, tolerating absence.
func Stop(ctx context.Context, d Docker, name string, grace time.Duration) error {
	if _, err := Inspect(ctx, d, name); isNotFound(err) {
		return nil
	}
	out, err := d.Run(ctx, "stop", "-t", fmt.Sprint(int(grace.Seconds())), name)
	if err != nil && !looksNotFound(out, err) {
		return fmt.Errorf("docker stop %s: %w (%s)", name, err, strings.TrimSpace(out))
	}
	return nil
}

func isNotFound(err error) bool {
	var nf *NotFoundError
	return errors.As(err, &nf)
}
