package auth

import (
	"crypto/tls"
	"errors"
	"io"
	"testing"
)

func handshake(t *testing.T, serverSecret, clientSecret string) error {
	t.Helper()
	srv, err := New(serverSecret)
	if err != nil {
		t.Fatal(err)
	}
	cli, err := New(clientSecret)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", srv.ServerConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	srvErr := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			srvErr <- err
			return
		}
		defer c.Close()
		err = c.(*tls.Conn).Handshake()
		if err == nil {
			_, err = c.Write([]byte("ok"))
		}
		srvErr <- err
	}()

	c, err := tls.Dial("tcp", ln.Addr().String(), cli.ClientConfig())
	if err == nil {
		// With TLS 1.3 the server verifies the client certificate after the
		// client's handshake returns; a read surfaces the server's verdict.
		_, err = io.ReadAll(io.LimitReader(c, 2))
		c.Close()
	}
	if sErr := <-srvErr; err == nil {
		err = sErr
	}
	return err
}

func TestSameSecretConnects(t *testing.T) {
	s := GenerateSecret()
	if err := handshake(t, s, s); err != nil {
		t.Fatalf("handshake with same secret: %v", err)
	}
}

func TestDifferentSecretRejected(t *testing.T) {
	err := handshake(t, GenerateSecret(), GenerateSecret())
	if err == nil {
		t.Fatal("handshake with different secrets succeeded")
	}
}

func TestWeakSecret(t *testing.T) {
	for _, s := range []string{"", "short"} {
		if _, err := New(s); !errors.Is(err, ErrWeakSecret) {
			t.Errorf("New(%q): want ErrWeakSecret, got %v", s, err)
		}
	}
}

func TestPlainTCPClientRejected(t *testing.T) {
	srv, err := New(GenerateSecret())
	if err != nil {
		t.Fatal(err)
	}
	cfg := srv.ServerConfig()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer c.Close()
		done <- c.(*tls.Conn).Handshake()
	}()
	// A TLS client without a certificate must be refused.
	if c, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{InsecureSkipVerify: true}); err == nil {
		_, _ = c.Read(make([]byte, 1))
		c.Close()
	}
	if sErr := <-done; sErr == nil {
		t.Fatal("server accepted a client without certificate")
	}
}
