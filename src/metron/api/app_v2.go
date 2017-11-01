package api

import (
	"diodes"
	"fmt"
	"log"
	"math/rand"
	"metric"

	clientpool "metron/clientpool/v2"
	egress "metron/egress/v2"
	ingress "metron/ingress/v2"
	v2 "plumbing/v2"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type AppV2 struct {
	config      *Config
	clientCreds credentials.TransportCredentials
	serverCreds credentials.TransportCredentials
}

func NewV2App(
	c *Config,
	clientCreds credentials.TransportCredentials,
	serverCreds credentials.TransportCredentials,
) *AppV2 {
	return &AppV2{
		config:      c,
		clientCreds: clientCreds,
		serverCreds: serverCreds,
	}
}

func (a *AppV2) Start() {
	if a.serverCreds == nil {
		log.Panic("Failed to load TLS server config")
	}

	envelopeBuffer := diodes.NewManyToOneEnvelopeV2(10000, diodes.AlertFunc(func(missed int) {
		// TODO: add tag "ingress"
		metric.IncCounter("dropped", metric.WithIncrement(uint64(missed)))
		log.Printf("Dropped %d v2 envelopes", missed)
	}))

	pool := a.initializePool()
	counterAggr := egress.New(pool)
	tx := egress.NewTransponder(envelopeBuffer, counterAggr)
	go tx.Start()

	metronAddress := fmt.Sprintf("127.0.0.1:%d", a.config.GRPC.Port)
	log.Printf("metron v2 API started on addr %s", metronAddress)
	rx := ingress.NewReceiver(envelopeBuffer)
	ingressServer := ingress.NewServer(metronAddress, rx, grpc.Creds(a.serverCreds))
	ingressServer.Start()
}

func (a *AppV2) initializePool() *clientpool.ClientPool {
	if a.clientCreds == nil {
		log.Panic("Failed to load TLS client config")
	}

	connector := clientpool.MakeGRPCConnector(
		a.config.DopplerAddr,
		a.config.DopplerAddrWithAZ,
		grpc.Dial,
		v2.NewDopplerIngressClient,
		grpc.WithTransportCredentials(a.clientCreds),
	)

	var connManagers []clientpool.Conn
	for i := 0; i < 5; i++ {
		connManagers = append(connManagers, clientpool.NewConnManager(connector, 10000+rand.Int63n(1000)))
	}

	return clientpool.New(connManagers...)
}
