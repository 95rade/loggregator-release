package v1

import (
	"context"
	"io"
	"log"
	"plumbing"
	"sync/atomic"
	"time"

	"github.com/cloudfoundry/dropsonde/metricbatcher"
	"github.com/cloudfoundry/sonde-go/events"
	"github.com/gogo/protobuf/proto"
)

type IngestorManager struct {
	sender  MessageSender
	batcher Batcher
}

type Batcher interface {
	BatchCounter(name string) metricbatcher.BatchCounterChainer
}

type MessageSender interface {
	Set(*events.Envelope)
}

type IngestorGRPCServer interface {
	plumbing.DopplerIngestor_PusherServer
}

func NewIngestor(sender MessageSender, batcher Batcher) *IngestorManager {
	return &IngestorManager{
		sender:  sender,
		batcher: batcher,
	}
}

func (i *IngestorManager) Pusher(pusher plumbing.DopplerIngestor_PusherServer) error {
	var done int64
	context := pusher.Context()
	go i.monitorContext(context, &done)

	for {
		if atomic.LoadInt64(&done) > 0 {
			return context.Err()
		}

		envelopeData, err := pusher.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		env := &events.Envelope{}
		err = proto.Unmarshal(envelopeData.Payload, env)
		if err != nil {
			log.Printf("Received bad envelope: %s", err)
			continue
		}
		i.batcher.BatchCounter("listeners.receivedEnvelopes").
			SetTag("protocol", "grpc").
			SetTag("event_type", env.GetEventType().String()).
			Increment()
		i.sender.Set(env)

		// metric-documentation-v1: (listeners.totalReceivedMessageCount) Total
		// number of messages received by doppler.
		i.batcher.BatchCounter("listeners.totalReceivedMessageCount").
			Increment()
	}
	return nil
}

func (i *IngestorManager) monitorContext(ctx context.Context, done *int64) {
	<-ctx.Done()
	atomic.StoreInt64(done, 1)
}
