package server

import (
	"github.com/stretchr/testify/assert"
	"testing"
)

type testAddr struct {
	addr string
}

func newAddr(addr string) testAddr {
	return testAddr{addr: addr}
}

func (a testAddr) Network() string {
	return "foo"
}
func (a testAddr) String() string {
	return a.addr
}

func TestPortFromAddr(t *testing.T) {
	port := ExtractPort(newAddr("[::]:12345"))
	assert.Equal(t, port, "12345")
}