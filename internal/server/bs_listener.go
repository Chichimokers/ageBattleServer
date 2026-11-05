package server

import (
	"context"
	"fmt"
	stdnet "net"

	"github.com/luskaner/ageBattleServer/internal/logger"
	battlenet "github.com/luskaner/ageBattleServer/internal/net"
)

type BsSocketListener struct {
	host     ClientHost
	counter  metricsCounter
	port     uint
	listener *battlenet.TCPListener
}

func NewBsSocketListener(host ClientHost, counter metricsCounter, port uint) *BsSocketListener {
	return &BsSocketListener{
		host:    host,
		counter: counter,
		port:    port,
	}
}

func (l *BsSocketListener) Start(ctx context.Context) error {
	addr := fmt.Sprintf(":%d", l.port)
	l.listener = battlenet.NewTCPListener(addr, l.handleConnection)

	if err := l.listener.Start(ctx); err != nil {
		return err
	}

	logger.Info("Listener", "listen", "bsPort listening on :%d", l.GetPort())
	return nil
}

func (l *BsSocketListener) handleConnection(conn battlenet.Conn) {
	tcpConn, ok := conn.(stdnet.Conn)
	if !ok {
		_ = conn.Close()
		return
	}
	//noinspection GoResourceLeak
	client := NewClient(tcpConn, l.host)
	client.setMetricsCounter(l.counter)
	if err := client.Start(context.Background()); err != nil {
		_ = client.Close()
	}
}

func (l *BsSocketListener) Stop() {
	if l.listener != nil {
		l.listener.Stop()
	}
}

func (l *BsSocketListener) GetPort() int {
	if l.listener != nil {
		return l.listener.GetPort()
	}
	// Not started, or stopped: there is no port to dial. The configured one is
	// not a fallback, because after Stop nothing is listening on it.
	return 0
}
