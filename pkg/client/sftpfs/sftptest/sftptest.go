// Package sftptest runs an SFTP server for tests: an SSH server in the test
// process, one account with a password, the sftp subsystem serving a
// directory on disk.
package sftptest

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// Server is the running server.
type Server struct {
	// Addr is host:port to dial.
	Addr string
	// HostKey is the server's key as an authorized_keys line, for pinning.
	HostKey string
	// User and Password log in.
	User, Password string

	ln net.Listener
	wg sync.WaitGroup
}

// New serves dir until the test ends. The server's working directory is dir,
// and paths are its own: absolute on the machine running the test.
func New(t testing.TB, dir string) *Server {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		HostKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))),
		User:    "reader", Password: "test-password",
	}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if c.User() == s.User && string(pass) == s.Password {
				return nil, nil
			}
			return nil, errors.New("denied")
		},
	}
	cfg.AddHostKey(signer)

	var lc net.ListenConfig
	s.ln, err = lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.Addr = s.ln.Addr().String()
	s.wg.Add(1)
	go s.accept(cfg, dir)
	t.Cleanup(func() {
		_ = s.ln.Close()
		s.wg.Wait()
	})
	return s
}

func (s *Server) accept(cfg *ssh.ServerConfig, dir string) {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serve(conn, cfg, dir)
		}()
	}
}

func (s *Server) serve(conn net.Conn, cfg *ssh.ServerConfig, dir string) {
	defer conn.Close()
	_, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "session only")
			continue
		}
		ch, requests, err := nc.Accept()
		if err != nil {
			return
		}
		go func() {
			for req := range requests {
				ok := req.Type == "subsystem" && len(req.Payload) > 4 && string(req.Payload[4:]) == "sftp"
				_ = req.Reply(ok, nil)
			}
		}()
		srv, err := sftp.NewServer(ch, sftp.WithServerWorkingDirectory(dir))
		if err != nil {
			_ = ch.Close()
			return
		}
		if err := srv.Serve(); err != nil && !errors.Is(err, io.EOF) {
			_ = srv.Close()
		}
		_ = ch.Close()
	}
}
