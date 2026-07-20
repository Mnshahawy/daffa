package sshx

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/ssh"
)

// RunScript pipes a shell script to `sh -s` on the remote host and streams its output line by line
// through onLine, returning the command's exit status. This is the ONLY place Daffa runs a shell on
// a machine it manages (docs/clusters.md §8, §11): everything else is the Docker API. It exists so a
// bare machine can have Docker installed over SSH, and nothing more.
//
// stdout and stderr are merged so the log reads in order. The caller's ctx bounds it: on cancel the
// session is torn down and ctx.Err() is returned.
func RunScript(ctx context.Context, client *ssh.Client, script string, onLine func(string)) error {
	sess, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("sshx: opening session: %w", err)
	}
	defer sess.Close()

	sess.Stdin = strings.NewReader(script)
	pr, pw := io.Pipe()
	sess.Stdout = pw
	sess.Stderr = pw

	if err := sess.Start("sh -s"); err != nil {
		return fmt.Errorf("sshx: starting shell: %w", err)
	}

	// Close the write end when the remote command finishes, so the scanner below unblocks.
	waitErr := make(chan error, 1)
	go func() {
		err := sess.Wait()
		_ = pw.Close()
		waitErr <- err
	}()

	// ctx cancellation tears the session down; the reader then hits EOF.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = sess.Signal(ssh.SIGTERM)
			_ = sess.Close()
		case <-stop:
		}
	}()

	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024) // a compose-plugin download can print long lines
	for sc.Scan() {
		onLine(sc.Text())
	}

	err = <-waitErr
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
