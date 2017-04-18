package groupedsinks

import (
	"doppler/groupedsinks/firehose_group"
	"doppler/groupedsinks/sink_wrapper"
	"doppler/sinks"
	"doppler/sinks/containermetric"
	"doppler/sinks/dump"
	"doppler/sinks/syslog"
	"doppler/sinks/websocket"
	"metric"
	"sync"

	"github.com/cloudfoundry/sonde-go/events"
)

type MetricBatcher interface {
	BatchIncrementCounter(name string)
}

func NewGroupedSinks(b MetricBatcher) *GroupedSinks {
	return &GroupedSinks{
		apps:      make(map[string]map[string]*sink_wrapper.SinkWrapper),
		firehoses: make(map[string]firehose_group.FirehoseGroup),
		batcher:   b,
	}
}

type GroupedSinks struct {
	apps      map[string]map[string]*sink_wrapper.SinkWrapper
	firehoses map[string]firehose_group.FirehoseGroup
	batcher   MetricBatcher
	sync.RWMutex
}

func (group *GroupedSinks) RegisterAppSink(in chan<- *events.Envelope, sink sinks.Sink) bool {
	group.Lock()
	defer group.Unlock()

	appId := sink.AppID()
	if appId == "" || sink.Identifier() == "" {
		return false
	}
	sinksForApp := group.apps[appId]
	if sinksForApp == nil {
		group.apps[appId] = make(map[string]*sink_wrapper.SinkWrapper)
		sinksForApp = group.apps[appId]
	}

	if _, ok := sinksForApp[sink.Identifier()]; ok {
		return false
	}
	sinksForApp[sink.Identifier()] = &sink_wrapper.SinkWrapper{InputChan: in, Sink: sink}
	return true
}

func (group *GroupedSinks) RegisterFirehoseSink(in chan<- *events.Envelope, sink sinks.Sink) bool {
	group.Lock()
	defer group.Unlock()

	subscriptionId := sink.AppID()
	if subscriptionId == "" {
		return false
	}

	fgroup := group.firehoses[subscriptionId]
	if fgroup == nil {
		group.firehoses[subscriptionId] = firehose_group.NewFirehoseGroup()
		fgroup = group.firehoses[subscriptionId]
	}

	return fgroup.AddSink(sink, in)
}

func (group *GroupedSinks) IsFirehoseRegistered(sink sinks.Sink) bool {
	group.RLock()
	defer group.RUnlock()
	subscriptionId := sink.AppID()
	if subscriptionId == "" {
		return false
	}

	fgroup := group.firehoses[subscriptionId]
	if fgroup == nil {
		return false
	}

	return fgroup.Exists(sink)
}

func (group *GroupedSinks) Broadcast(appId string, msg *events.Envelope) {
	group.RLock()
	defer group.RUnlock()

	for _, wrapper := range group.apps[appId] {
		select {
		case wrapper.InputChan <- msg:
		default:
			// metric-documentation-v1: (sinks.dropped) Number of envelopes dropped
			// while inserting envelope into sink.
			group.batcher.BatchIncrementCounter("sinks.dropped")

			// metric-documentation-v2: (loggregator.doppler.sinks.dropped)
			// Number of envelopes dropped while inserting envelope into sink.
			metric.IncCounter("sinks.dropped")
		}
	}

	group.BroadcastMessageToFirehoses(msg)
}

func (group *GroupedSinks) BroadcastError(appId string, errorMsg *events.Envelope) {
	group.RLock()
	defer group.RUnlock()

	for _, wrapper := range group.apps[appId] {
		if wrapper.Sink.ShouldReceiveErrors() {
			select {
			case wrapper.InputChan <- errorMsg:
			default:
				// metric-documentation-v1: (sinks.errors.dropped) Number of errors dropped
				// while inserting error into sink.
				group.batcher.BatchIncrementCounter("sinks.errors.dropped")

				// metric-documentation-v2: (loggregator.doppler.sinks.errors.dropped)
				// Number of errors dropped while inserting error into sink.
				metric.IncCounter("sinks.errors.dropped")
			}
		}
	}

	group.BroadcastMessageToFirehoses(errorMsg)
}

func (group *GroupedSinks) BroadcastMessageToFirehoses(msg *events.Envelope) {
	for _, fgroup := range group.firehoses {
		fgroup.BroadcastMessage(msg)
	}
}

func (group *GroupedSinks) CountFor(appId string) int {
	group.RLock()
	defer group.RUnlock()

	if _, ok := group.apps[appId]; !ok {
		return 0
	}
	return len(group.apps[appId])
}

func (group *GroupedSinks) DrainFor(appId, drainMetaData string) sinks.Sink {
	group.RLock()
	defer group.RUnlock()

	wrapper, ok := group.apps[appId][drainMetaData]
	if ok {
		return wrapper.Sink
	}
	return nil
}

func (group *GroupedSinks) DrainsFor(appId string) []sinks.Sink {
	group.RLock()
	defer group.RUnlock()

	results := []sinks.Sink{}
	for _, wrapper := range group.apps[appId] {
		_, isSyslogSink := wrapper.Sink.(*syslog.SyslogSink)
		if isSyslogSink {
			results = append(results, wrapper.Sink)
		}
	}

	return results
}

func (group *GroupedSinks) DumpFor(appId string) *dump.DumpSink {
	group.RLock()
	defer group.RUnlock()

	appCache, ok := group.apps[appId]

	if !ok {
		return nil
	}
	if _, ok := appCache[appId]; !ok {

		return nil
	}
	return appCache[appId].Sink.(*dump.DumpSink)
}

func (group *GroupedSinks) ContainerMetricsFor(appId string) *containermetric.ContainerMetricSink {
	group.RLock()
	defer group.RUnlock()

	appCache, ok := group.apps[appId]

	if !ok {
		return nil
	}

	sinkId := "container-metrics-" + appId
	if _, ok := appCache[sinkId]; !ok {
		return nil
	}

	return appCache[sinkId].Sink.(*containermetric.ContainerMetricSink)
}

func (group *GroupedSinks) WebsocketSinksFor(appId string) []websocket.WebsocketSink {
	results := []websocket.WebsocketSink{}

	group.RLock()
	group.RUnlock()

	for _, wrapper := range group.apps[appId] {
		webSocketSink, isWebsocketSink := wrapper.Sink.(*websocket.WebsocketSink)
		if isWebsocketSink {
			results = append(results, *webSocketSink)
		}
	}

	return results
}

func (group *GroupedSinks) CloseAndDelete(sink sinks.Sink) bool {
	group.Lock()
	defer group.Unlock()

	appId := sink.AppID()
	wrapper, ok := group.apps[appId][sink.Identifier()]
	if ok {
		close(wrapper.InputChan)
		delete(group.apps[appId], sink.Identifier())
		return true
	}
	return false
}

func (group *GroupedSinks) CloseAndDeleteFirehose(sink sinks.Sink) bool {
	group.Lock()
	defer group.Unlock()
	firehoseSubscriptionId := sink.AppID()
	fgroup, ok := group.firehoses[firehoseSubscriptionId]
	if !ok {
		return false
	}

	removed := fgroup.RemoveSink(sink)

	if removed == false {
		return false
	}

	if fgroup.IsEmpty() == true {
		delete(group.firehoses, firehoseSubscriptionId)
	}

	return true
}

func (group *GroupedSinks) DeleteAll() {
	group.Lock()
	defer group.Unlock()
	for appId, appSinks := range group.apps {
		for _, wrapper := range appSinks {
			close(wrapper.InputChan)
		}
		delete(group.apps, appId)
	}
	for subscriptionId, fgroup := range group.firehoses {
		fgroup.RemoveAllSinks()
		delete(group.firehoses, subscriptionId)
	}
}
