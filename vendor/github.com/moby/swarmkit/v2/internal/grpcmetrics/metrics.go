package grpcmetrics

import (
	grpcprom "github.com/grpc-ecosystem/go-grpc-middleware/providers/prometheus"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	Client = grpcprom.NewClientMetrics()
	Server = grpcprom.NewServerMetrics()
)

func init() {
	prometheus.MustRegister(Client, Server)
}
