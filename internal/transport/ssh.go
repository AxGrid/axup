package transport

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

type Client struct {
	host Host
	c    *ssh.Client
}

// Dial opens an SSH connection. The caller passes a "[user@]host[:port]" spec
// and credential options. When both KeyPath and Password are empty in opts,
// Dial auto-discovers credentials from ssh-agent + ~/.ssh.
func Dial(spec string, opts AuthOptions) (*Client, error) {
	host, err := ParseHost(spec)
	if err != nil {
		return nil, err
	}
	auth, err := buildAuthMethods(opts)
	if err != nil {
		return nil, err
	}
	hk, err := resolveHostKey(host)
	if err != nil {
		return nil, err
	}
	cfg := &ssh.ClientConfig{
		User:              host.User,
		Auth:              auth,
		HostKeyCallback:   hk.callback,
		HostKeyAlgorithms: hk.algorithms,
		Timeout:           15 * time.Second,
	}
	c, err := ssh.Dial("tcp", host.HostPort(), cfg)
	if err != nil {
		return nil, fmt.Errorf("ssh dial %s: %w", host.Pretty(), err)
	}
	return &Client{host: host, c: c}, nil
}

func (c *Client) Host() Host  { return c.host }
func (c *Client) Close() error { return c.c.Close() }

// Run executes a shell command on the remote and returns stdout/stderr. Useful
// for short fact-finding commands like `uname -m`.
func (c *Client) Run(cmd string) (stdout, stderr string, err error) {
	sess, err := c.c.NewSession()
	if err != nil {
		return "", "", err
	}
	defer sess.Close()
	var so, se bytes.Buffer
	sess.Stdout = &so
	sess.Stderr = &se
	err = sess.Run(cmd)
	return so.String(), se.String(), err
}

// DetectArch returns the remote machine's architecture in Go's GOARCH terms.
func (c *Client) DetectArch() (string, error) {
	out, errOut, err := c.Run("uname -m")
	if err != nil {
		return "", fmt.Errorf("uname -m: %w (stderr: %s)", err, strings.TrimSpace(errOut))
	}
	switch strings.TrimSpace(out) {
	case "x86_64", "amd64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("unsupported remote arch: %q", strings.TrimSpace(out))
	}
}

// UploadBinary writes `data` to remotePath on the server and chmods it +x.
// Uses `cat > path && chmod +x path` over a single SSH session with the data
// piped through stdin. Avoids pulling in an SFTP dependency for MVP.
func (c *Client) UploadBinary(data []byte, remotePath string) error {
	sess, err := c.c.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	sess.Stdin = bytes.NewReader(data)
	var stderr bytes.Buffer
	sess.Stderr = &stderr
	cmd := fmt.Sprintf("cat > %s && chmod 0755 %s", shellQuote(remotePath), shellQuote(remotePath))
	if err := sess.Run(cmd); err != nil {
		return fmt.Errorf("upload %s: %w (stderr: %s)", remotePath, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// AgentExec controls how RunAgent invokes the uploaded binary on the remote.
// When Sudo is true the command is wrapped in `sudo -H -S -p ''`; when
// SudoPassword is non-empty it is written to the session's stdin before the
// plan JSON (sudo consumes the line, the agent reads the rest).
type AgentExec struct {
	Sudo         bool
	SudoPassword string
}

// RunAgent invokes the uploaded agent. It pipes `planJSON` to the agent's
// stdin (prefixed with the sudo password when needed), streams each line of
// the agent's stdout to `onLine`, and propagates any agent stderr noise into
// the returned error.
func (c *Client) RunAgent(remotePath string, planJSON []byte, exec AgentExec, onLine func([]byte)) error {
	sess, err := c.c.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()

	cmd := shellQuote(remotePath)
	var stdin bytes.Buffer
	if exec.Sudo {
		// -H sets HOME to the target user (root), so the agent's state path
		// lands in /root/.axup-state regardless of the SSH login user.
		// -S reads the password from stdin, -p '' suppresses the prompt
		// (we already know we're piping).
		cmd = "sudo -H -S -p '' " + cmd
		if exec.SudoPassword != "" {
			stdin.WriteString(exec.SudoPassword)
			stdin.WriteByte('\n')
		}
	}
	stdin.Write(planJSON)
	sess.Stdin = &stdin

	stdoutPipe, err := sess.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	sess.Stderr = &stderr

	if err := sess.Start(cmd); err != nil {
		return fmt.Errorf("start agent: %w", err)
	}

	// Stream stdout line-by-line.
	readErr := streamLines(stdoutPipe, onLine)

	if err := sess.Wait(); err != nil {
		return fmt.Errorf("agent exited: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	if readErr != nil {
		return fmt.Errorf("read agent stdout: %w", readErr)
	}
	if s := strings.TrimSpace(stderr.String()); s != "" {
		// Agent finished cleanly but printed to stderr — surface it but do not fail.
		fmt.Fprintf(os.Stderr, "[%s] agent stderr: %s\n", c.host.Pretty(), s)
	}
	return nil
}

func streamLines(r io.Reader, onLine func([]byte)) error {
	buf := make([]byte, 0, 4096)
	chunk := make([]byte, 4096)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			for {
				i := bytes.IndexByte(buf, '\n')
				if i < 0 {
					break
				}
				line := buf[:i]
				if len(line) > 0 {
					onLine(line)
				}
				buf = buf[i+1:]
			}
		}
		if err == io.EOF {
			if len(buf) > 0 {
				onLine(buf)
			}
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// Stream runs cmd on the remote and forwards its stdout/stderr to the
// provided writers as bytes arrive. Returns when the remote process
// exits OR when ctx is cancelled (whichever comes first). On
// cancellation the SSH session is closed, which kills the remote
// process. Used by `axup logs` for tail -F streaming.
func (c *Client) Stream(ctx context.Context, cmd string, stdout, stderr io.Writer) error {
	sess, err := c.c.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	if stdout != nil {
		sess.Stdout = stdout
	}
	if stderr != nil {
		sess.Stderr = stderr
	}
	if err := sess.Start(cmd); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	select {
	case <-ctx.Done():
		// Closing the session sends SIGHUP to the remote process group;
		// `tail` exits cleanly. We swallow Wait's error after cancel.
		_ = sess.Close()
		<-done
		return ctx.Err()
	case err := <-done:
		return err
	}
}

// shellQuote wraps s in single quotes safe for /bin/sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
