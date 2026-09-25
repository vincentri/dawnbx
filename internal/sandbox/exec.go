package sandbox

import (
	"context"
	"errors"
	"io"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"
)

// kubeExec runs cmd in the sandbox's main container and returns its exit code.
// A nonzero exit is a result, not an error.
func (m *Manager) kubeExec(ctx context.Context, id string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	return m.stream(ctx, id, cmd, remotecommand.StreamOptions{Stdin: stdin, Stdout: stdout, Stderr: stderr})
}

// kubeTTY is kubeExec with a terminal: stderr merges into out, sizes carries resizes.
func (m *Manager) kubeTTY(ctx context.Context, id string, cmd []string, in io.Reader, out io.Writer, sizes remotecommand.TerminalSizeQueue) (int, error) {
	return m.stream(ctx, id, cmd, remotecommand.StreamOptions{Stdin: in, Stdout: out, Tty: true, TerminalSizeQueue: sizes})
}

func (m *Manager) stream(ctx context.Context, id string, cmd []string, o remotecommand.StreamOptions) (int, error) {
	req := m.Kube.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(Namespace).Name(id).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "main", Command: cmd,
			Stdin: o.Stdin != nil, Stdout: true, Stderr: !o.Tty, TTY: o.Tty,
		}, scheme.ParameterCodec)
	ws, err := remotecommand.NewWebSocketExecutor(m.Rest, "GET", req.URL().String())
	if err != nil {
		return 0, err
	}
	spdy, err := remotecommand.NewSPDYExecutor(m.Rest, "POST", req.URL())
	if err != nil {
		return 0, err
	}
	ex, err := remotecommand.NewFallbackExecutor(ws, spdy, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})
	if err != nil {
		return 0, err
	}
	err = ex.StreamWithContext(ctx, o)
	var ee utilexec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitStatus(), nil
	}
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, errf(503, "exec_failed", "the sandbox may be restarting; retry, or check sb.get() status", "exec failed: %v", err)
	}
	return 0, nil
}

// A login shell in /workspace; bash when the image has it.
var termCmd = []string{"env", "TERM=xterm-256color", "sh", "-c", "command -v bash >/dev/null && exec bash -l || exec sh -l"}

// Terminal runs an interactive shell in a running sandbox until it exits or ctx ends.
func (m *Manager) Terminal(ctx context.Context, id string, in io.Reader, out io.Writer, sizes remotecommand.TerminalSizeQueue) error {
	if _, err := m.running(id); err != nil {
		return err
	}
	_, err := m.RunTTY(ctx, id, termCmd, in, out, sizes)
	return err
}
