package helpergui

import (
	"context"
	"net"
	"testing"

	"tomcat-ops-helper/internal/config"
)

func TestDBTestClassifiesValidateAndConnect(t *testing.T) {
	// Incomplete block → validate.
	r := TestDB(context.Background(), &config.DBConfig{Host: "", User: "u", Password: "p", Database: "d"})
	if r.OK || r.ErrorKind != "validate" {
		t.Fatalf("want validate, got %+v", r)
	}

	// Structurally complete but nothing listening → connect.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close() // freed port: connection refused, fails fast
	r = TestDB(context.Background(), &config.DBConfig{
		Host: "127.0.0.1", Port: port, User: "u", Password: "p", Database: "d",
	})
	if r.OK || r.ErrorKind != "connect" {
		t.Fatalf("want connect, got %+v", r)
	}
	if r.Driver != "mysql" {
		t.Fatalf("empty type must default to mysql, got %q", r.Driver)
	}
}
