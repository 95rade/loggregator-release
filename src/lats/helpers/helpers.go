package helpers

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"plumbing"
	"strconv"
	"time"

	v2 "plumbing/v2"

	"google.golang.org/grpc"

	"code.cloudfoundry.org/workpool"

	. "github.com/onsi/gomega"

	. "lats/config"

	"github.com/cloudfoundry/dropsonde/envelope_extensions"
	"github.com/cloudfoundry/noaa/consumer"
	"github.com/cloudfoundry/sonde-go/events"
	"github.com/cloudfoundry/storeadapter"
	"github.com/cloudfoundry/storeadapter/etcdstoreadapter"
)

const ORIGIN_NAME = "LATs"

var config *TestConfig

func Initialize(testConfig *TestConfig) {
	config = testConfig
}

func ConnectToStream(appID string) (<-chan *events.Envelope, <-chan error) {
	connection, printer := SetUpConsumer()
	msgChan, errorChan := connection.Stream(appID, "")

	readErrs := func() error {
		select {
		case err := <-errorChan:
			return err
		default:
			return nil
		}
	}

	Consistently(readErrs).Should(BeNil())
	WaitForWebsocketConnection(printer)

	return msgChan, errorChan
}

func ConnectToFirehose() (<-chan *events.Envelope, <-chan error) {
	connection, printer := SetUpConsumer()
	randomString := strconv.FormatInt(time.Now().UnixNano(), 10)
	subscriptionId := "firehose-" + randomString[len(randomString)-5:]

	msgChan, errorChan := connection.Firehose(subscriptionId, "")

	readErrs := func() error {
		select {
		case err := <-errorChan:
			return err
		default:
			return nil
		}
	}

	Consistently(readErrs).Should(BeNil())
	WaitForWebsocketConnection(printer)

	return msgChan, errorChan
}

func RequestContainerMetrics(appID string) ([]*events.ContainerMetric, error) {
	consumer, _ := SetUpConsumer()
	return consumer.ContainerMetrics(appID, "")
}

func RequestRecentLogs(appID string) ([]*events.LogMessage, error) {
	consumer, _ := SetUpConsumer()
	return consumer.RecentLogs(appID, "")
}

func SetUpConsumer() (*consumer.Consumer, *TestDebugPrinter) {
	tlsConfig := tls.Config{InsecureSkipVerify: config.SkipSSLVerify}
	printer := &TestDebugPrinter{}

	connection := consumer.New(config.DopplerEndpoint, &tlsConfig, nil)
	connection.SetDebugPrinter(printer)
	return connection, printer
}

func WaitForWebsocketConnection(printer *TestDebugPrinter) {
	Eventually(printer.Dump, 2*time.Second).Should(ContainSubstring("101 Switching Protocols"))
}

func EmitToMetron(envelope *events.Envelope) {
	metronConn, err := net.Dial("udp4", fmt.Sprintf("localhost:%d", config.DropsondePort))
	Expect(err).NotTo(HaveOccurred())

	b, err := envelope.Marshal()
	Expect(err).NotTo(HaveOccurred())

	_, err = metronConn.Write(b)
	Expect(err).NotTo(HaveOccurred())
}

func EmitToMetronV2(envelope *v2.Envelope) {
	creds, err := plumbing.NewServerCredentials(
		config.MetronTLSClientConfig.CertFile,
		config.MetronTLSClientConfig.KeyFile,
		config.MetronTLSClientConfig.CAFile,
		"metron",
	)
	Expect(err).NotTo(HaveOccurred())

	conn, err := grpc.Dial("localhost:3458", grpc.WithTransportCredentials(creds))
	c := v2.NewIngressClient(conn)

	s, err := c.Sender(context.Background())
	Expect(err).NotTo(HaveOccurred())
	defer s.CloseSend()

	for i := 0; i < WriteCount; i++ {
		err = s.Send(envelope)
		Expect(err).NotTo(HaveOccurred())
	}
}

func ReadFromRLP(appID string) <-chan *v2.Envelope {
	creds, err := plumbing.NewServerCredentials(
		config.MetronTLSClientConfig.CertFile,
		config.MetronTLSClientConfig.KeyFile,
		config.MetronTLSClientConfig.CAFile,
		"reverselogproxy",
	)
	Expect(err).NotTo(HaveOccurred())

	conn, err := grpc.Dial(config.ReverseLogProxyAddr, grpc.WithTransportCredentials(creds))
	Expect(err).NotTo(HaveOccurred())

	client := v2.NewEgressClient(conn)
	receiver, err := client.Receiver(context.Background(), &v2.EgressRequest{
		ShardId: fmt.Sprint("shard-", time.Now().UnixNano()),
		Filter: &v2.Filter{
			SourceId: appID,
			Message: &v2.Filter_Log{
				Log: &v2.LogFilter{},
			},
		},
	})
	Expect(err).ToNot(HaveOccurred())

	msgChan := make(chan *v2.Envelope, 100)

	go func() {
		defer conn.Close()
		for {
			e, err := receiver.Recv()
			if err != nil {
				break
			}

			msgChan <- e
		}
	}()

	return msgChan
}

func ReadContainerFromRLP(appID string) []*v2.Envelope {
	creds, err := plumbing.NewServerCredentials(
		config.MetronTLSClientConfig.CertFile,
		config.MetronTLSClientConfig.KeyFile,
		config.MetronTLSClientConfig.CAFile,
		"reverselogproxy",
	)
	Expect(err).NotTo(HaveOccurred())

	conn, err := grpc.Dial(config.ReverseLogProxyAddr, grpc.WithTransportCredentials(creds))
	Expect(err).NotTo(HaveOccurred())

	client := v2.NewEgressQueryClient(conn)

	ctx, _ := context.WithTimeout(context.Background(), time.Second)
	resp, err := client.ContainerMetrics(ctx, &v2.ContainerMetricRequest{
		SourceId: appID,
	})
	Expect(err).ToNot(HaveOccurred())
	return resp.Envelopes
}

func FindMatchingEnvelope(msgChan <-chan *events.Envelope, envelope *events.Envelope) *events.Envelope {
	timeout := time.After(10 * time.Second)
	for {
		select {
		case receivedEnvelope := <-msgChan:
			if receivedEnvelope.GetTags()["UniqueName"] == envelope.GetTags()["UniqueName"] {
				return receivedEnvelope
			}
		case <-timeout:
			return nil
		}
	}
}

func FindMatchingEnvelopeByOrigin(msgChan <-chan *events.Envelope, origin string) *events.Envelope {
	timeout := time.After(10 * time.Second)
	for {
		select {
		case receivedEnvelope := <-msgChan:
			if receivedEnvelope.GetOrigin() == origin {
				return receivedEnvelope
			}
		case <-timeout:
			return nil
		}
	}
}

func FindMatchingEnvelopeByID(id string, msgChan <-chan *events.Envelope) (*events.Envelope, error) {
	timeout := time.After(10 * time.Second)
	for {
		select {
		case receivedEnvelope := <-msgChan:
			receivedID := envelope_extensions.GetAppId(receivedEnvelope)
			if receivedID == id {
				return receivedEnvelope, nil
			}
			return nil, fmt.Errorf("Expected messages with app id: %s, got app id: %s", id, receivedID)
		case <-timeout:
			return nil, errors.New("Timed out while waiting for message")
		}
	}
}

func WriteToEtcd(urls []string, key, value string) func() {
	etcdOptions := &etcdstoreadapter.ETCDOptions{
		IsSSL:       true,
		CertFile:    config.EtcdTLSClientConfig.CertFile,
		KeyFile:     config.EtcdTLSClientConfig.KeyFile,
		CAFile:      config.EtcdTLSClientConfig.CAFile,
		ClusterUrls: urls,
	}

	workPool, err := workpool.NewWorkPool(10)
	Expect(err).NotTo(HaveOccurred())
	adapter, err := etcdstoreadapter.New(etcdOptions, workPool)
	Expect(err).NotTo(HaveOccurred())
	err = adapter.Create(storeadapter.StoreNode{
		Key:   key,
		Value: []byte(value),
		TTL:   uint64(time.Minute),
	})
	Expect(err).NotTo(HaveOccurred())

	return func() {
		err = adapter.Delete(key)
		Expect(err).ToNot(HaveOccurred())
	}
}
