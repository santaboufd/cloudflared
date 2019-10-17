//+build windows

package sshserver

import (
	"errors"

	"time"

	"github.com/cloudflare/cloudflared/sshlog"
	"github.com/sirupsen/logrus"
)

const SSHPreambleLength = 2

type SSHServer struct{}

type SSHPreamble struct {
	Destination string
	JWT         string
}

func New(_ sshlog.Manager, _ *logrus.Logger, _, _, _, _ string, _ chan struct{}, _, _ time.Duration) (*SSHServer, error) {
	return nil, errors.New("cloudflared ssh server is not supported on windows")
}

func (s *SSHServer) Start() error {
	return errors.New("cloudflared ssh server is not supported on windows")
}
