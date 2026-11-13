// Package sftpfs reads a directory tree on an SFTP server: it lists a
// directory, describes a path and reads a file. That is all of it — there is
// no code here that writes, renames or deletes, whatever the account on the
// other side may do.
//
// The server is checked against a pinned host key: a key learnt on first use
// is a key nobody checked.
package sftpfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// Config is one server and one account on it.
type Config struct {
	// Addr is host:port.
	Addr string
	User string
	// Password or Key (a private key in PEM); one of them.
	Password string
	Key      []byte
	// HostKey is the server's public key as an authorized_keys line.
	HostKey string
	// Timeout bounds the dial and the handshake; required. What comes after
	// is bounded by the context Dial was given.
	Timeout time.Duration
}

// Conn is an open session. It is closed when the context it was dialled with
// is done, so a call that times out does not leave it hanging.
type Conn struct {
	ssh  *ssh.Client
	sftp *sftp.Client
	stop func() bool
}

// Dial connects, checks the host key, logs in and starts the sftp subsystem.
func Dial(ctx context.Context, cfg Config) (*Conn, error) {
	hostKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(cfg.HostKey))
	if err != nil {
		return nil, fmt.Errorf("sftp: host key: %w", err)
	}
	var auth ssh.AuthMethod
	switch {
	case len(cfg.Key) > 0:
		signer, keyErr := ssh.ParsePrivateKey(cfg.Key)
		if keyErr != nil {
			return nil, fmt.Errorf("sftp: private key: %w", keyErr)
		}
		auth = ssh.PublicKeys(signer)
	default:
		auth = ssh.Password(cfg.Password)
	}
	sshCfg := &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            []ssh.AuthMethod{auth},
		HostKeyCallback: ssh.FixedHostKey(hostKey),
		Timeout:         cfg.Timeout,
	}

	d := net.Dialer{Timeout: cfg.Timeout}
	raw, err := d.DialContext(ctx, "tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("sftp: dial %s: %w", cfg.Addr, err)
	}
	// The handshake has no context of its own: a deadline on the socket
	// stands in for it — the context's or Timeout, whichever comes first —
	// lifted once the session is up. After that the context closes the
	// session when it is done.
	deadline := time.Now().Add(cfg.Timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = raw.SetDeadline(deadline)
	sc, chans, reqs, err := ssh.NewClientConn(raw, cfg.Addr, sshCfg)
	if err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("sftp: handshake with %s: %w", cfg.Addr, err)
	}
	client := ssh.NewClient(sc, chans, reqs)
	sc2, err := sftp.NewClient(client)
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("sftp: subsystem: %w", err)
	}
	_ = raw.SetDeadline(time.Time{})
	c := &Conn{ssh: client, sftp: sc2}
	c.stop = context.AfterFunc(ctx, func() { _ = c.close() })
	return c, nil
}

// Close ends the session.
func (c *Conn) Close() error {
	c.stop()
	return c.close()
}

func (c *Conn) close() error {
	return errors.Join(c.sftp.Close(), c.ssh.Close())
}

// Lstat describes a path without following it: a symlink is reported as one.
func (c *Conn) Lstat(path string) (os.FileInfo, error) {
	return c.sftp.Lstat(path)
}

// ReadDir lists a directory, its entries described as Lstat would.
func (c *Conn) ReadDir(path string) ([]os.FileInfo, error) {
	return c.sftp.ReadDir(path)
}

// ErrTooLarge is a file longer than the limit it was read with.
var ErrTooLarge = errors.New("sftp: file is over the limit")

// ReadFile reads a file whole, stopping past limit bytes.
func (c *Conn) ReadFile(path string, limit int64) ([]byte, error) {
	f, err := c.sftp.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, ErrTooLarge
	}
	return data, nil
}
