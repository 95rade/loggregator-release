package v2_test

import (
	"doppler/grpcmanager/v2"
	"io"
	plumbing "plumbing/v2"

	"github.com/cloudfoundry/dropsonde/metricbatcher"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Ingress", func() {
	var (
		mockDataSetter *mockDataSetter
		mockSender     *mockDopplerIngress_SenderServer

		ingestor *v2.Ingestor
	)

	BeforeEach(func() {
		mockDataSetter = newMockDataSetter()
		mockSender = newMockDopplerIngress_SenderServer()

		ingestor = v2.NewIngestor(mockDataSetter, SpyBatcher{})
	})

	It("writes the v2 envelope as a v1 envelope to data setter", func() {
		mockSender.RecvOutput.Ret0 <- &plumbing.Envelope{
			Message: &plumbing.Envelope_Log{
				Log: &plumbing.Log{
					Payload: []byte("hello"),
				},
			},
		}
		mockSender.RecvOutput.Ret1 <- nil
		mockSender.RecvOutput.Ret0 <- nil
		mockSender.RecvOutput.Ret1 <- io.EOF

		ingestor.Sender(mockSender)
		Expect(mockDataSetter.SetCalled).To(HaveLen(1))
	})

	It("throws invalid envelopes on the ground", func() {
		mockSender.RecvOutput.Ret0 <- &plumbing.Envelope{}
		mockSender.RecvOutput.Ret1 <- nil
		mockSender.RecvOutput.Ret0 <- nil
		mockSender.RecvOutput.Ret1 <- io.EOF

		ingestor.Sender(mockSender)
		Expect(mockDataSetter.SetCalled).To(HaveLen(0))
	})
})

type SpyBatcher struct {
	metricbatcher.BatchCounterChainer
}

func (s SpyBatcher) BatchCounter(string) metricbatcher.BatchCounterChainer {
	return s
}

func (s SpyBatcher) Increment() {
}
