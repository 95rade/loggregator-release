package v2

import (
	"log"
	"metricemitter"
	plumbing "plumbing/v2"
	"time"
)

type Nexter interface {
	TryNext() (*plumbing.Envelope, bool)
}

type Writer interface {
	Write(msgs []*plumbing.Envelope) error
}

type Transponder struct {
	nexter        Nexter
	writer        Writer
	tags          map[string]string
	batchSize     int
	batchInterval time.Duration
	droppedMetric *metricemitter.Counter
	egressMetric  *metricemitter.Counter
}

func NewTransponder(
	n Nexter,
	w Writer,
	tags map[string]string,
	batchSize int,
	batchInterval time.Duration,
	metricClient metricemitter.MetricClient,
) *Transponder {
	droppedMetric := metricClient.NewCounter("dropped",
		metricemitter.WithVersion(2, 0),
		metricemitter.WithTags(map[string]string{"direction": "egress"}),
	)

	egressMetric := metricClient.NewCounter("dropped",
		metricemitter.WithVersion(2, 0),
	)

	return &Transponder{
		nexter:        n,
		writer:        w,
		tags:          tags,
		batchSize:     batchSize,
		batchInterval: batchInterval,
		droppedMetric: droppedMetric,
		egressMetric:  egressMetric,
	}
}

func (t *Transponder) Start() {
	var batch []*plumbing.Envelope
	lastSent := time.Now()

	for {
		envelope, ok := t.nexter.TryNext()
		if !ok && !t.batchReady(batch, lastSent) {
			time.Sleep(10 * time.Millisecond)
			continue
		}

		if ok {
			t.addTags(envelope)
			batch = append(batch, envelope)
		}

		if !t.batchReady(batch, lastSent) {
			continue
		}

		err := t.writer.Write(batch)
		batch = nil
		lastSent = time.Now()
		if err != nil {
			// metric-documentation-v2: (loggregator.metron.dropped) Number of messages
			// dropped when failing to write to Dopplers v2 API
			t.droppedMetric.Increment(uint64(len(batch)))
			log.Printf("v2 egress dropped: %s", err)
			continue
		}

		// metric-documentation-v2: (loggregator.metron.egress)
		// Number of messages written to Doppler's v2 API
		t.egressMetric.Increment(uint64(len(batch)))
	}
}

func (t *Transponder) batchReady(batch []*plumbing.Envelope, lastSent time.Time) bool {
	if len(batch) == 0 {
		return false
	}

	return len(batch) >= t.batchSize || time.Since(lastSent) >= t.batchInterval
}

func (t *Transponder) addTags(e *plumbing.Envelope) {
	if e.DeprecatedTags == nil {
		e.DeprecatedTags = make(map[string]*plumbing.Value)
	}
	for k, v := range t.tags {
		if _, ok := e.DeprecatedTags[k]; !ok {
			e.DeprecatedTags[k] = &plumbing.Value{
				Data: &plumbing.Value_Text{
					Text: v,
				},
			}
		}
	}
}
