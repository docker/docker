package main

import (
	"net"
	"net/http"

	"github.com/docker/go-metrics"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

func (cli *DaemonCli) startMetricsServer(addr string) error {
	if addr == "" {
		return nil
	}

	if !cli.d.HasExperimental() {
		return errors.New("metrics-addr is only supported when experimental is enabled")
	}

	if err := allocateDaemonPort(addr); err != nil {
		return err
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.Handler())
	go func() {
		if err := http.Serve(l, mux); err != nil {
			logrus.Errorf("serve metrics api: %s", err)
		}
	}()
	return nil
}
