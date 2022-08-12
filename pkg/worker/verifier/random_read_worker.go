package verifier

import (
	"context"
	"math/rand"
	"runtime"
	"time"

	"github.com/redpanda-data/kgo-verifier/pkg/util"
	log "github.com/sirupsen/logrus"
	"github.com/twmb/franz-go/pkg/kgo"

	worker "github.com/redpanda-data/kgo-verifier/pkg/worker"
)

type RandomReadConfig struct {
	workerCfg   worker.WorkerConfig
	name        string
	readCount   int
	nPartitions int32
}

type RandomReadWorker struct {
	config RandomReadConfig
	Status WorkerStatus
}

type WorkerStatus struct {
	Validator ValidatorStatus `json:"validator"`
	Errors    int             `json:"errors"`
}

func NewRandomReadConfig(wc worker.WorkerConfig, name string, nPartitions int32) RandomReadConfig {
	return RandomReadConfig{
		workerCfg:   wc,
		name:        name,
		nPartitions: nPartitions,
	}
}

func NewRandomReadWorker(cfg RandomReadConfig) RandomReadWorker {
	return RandomReadWorker{
		config: cfg,
		Status: WorkerStatus{},
	}
}

func (w *RandomReadWorker) newClient(opts []kgo.Opt) *kgo.Client {
	opts = append(opts, w.config.workerCfg.MakeKgoOpts()...)

	client, err := kgo.NewClient(opts...)
	util.Chk(err, "Error creating kafka client")
	return client
}

func (w *RandomReadWorker) Wait() {
	// Basic client to read offsets
	client := w.newClient(make([]kgo.Opt, 0))
	endOffsets := GetOffsets(client, w.config.workerCfg.Topic, w.config.nPartitions, -1)
	client.Close()
	client = w.newClient(make([]kgo.Opt, 0))
	startOffsets := GetOffsets(client, w.config.workerCfg.Topic, w.config.nPartitions, -2)
	client.Close()
	runtime.GC()

	validRanges := LoadTopicOffsetRanges(w.config.workerCfg.Topic, w.config.nPartitions)

	ctxLog := log.WithFields(log.Fields{"tag": w.config.name})

	readCount := w.config.readCount

	// Select a partition and location
	ctxLog.Infof("Reading %d random offsets", w.config.readCount)

	i := 0
	for i < readCount {
		p := rand.Int31n(w.config.nPartitions)
		pStart := startOffsets[p]
		pEnd := endOffsets[p]

		if pEnd-pStart < 2 {
			ctxLog.Warnf("Partition %d is empty, skipping read", p)
			continue
		}
		o := rand.Int63n(pEnd-pStart-1) + pStart
		offset := kgo.NewOffset().At(o)

		// Construct a map of topic->partition->offset to seek our new client to the right place
		offsets := make(map[string]map[int32]kgo.Offset)
		partOffsets := make(map[int32]kgo.Offset, 1)
		partOffsets[p] = offset
		offsets[w.config.workerCfg.Topic] = partOffsets

		// Fully-baked client for actual consume
		opts := []kgo.Opt{
			kgo.ConsumePartitions(offsets),
		}

		client = w.newClient(opts)

		// Read one record
		ctxLog.Debugf("Reading partition %d (%d-%d) at offset %d", p, pStart, pEnd, offset)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
		defer cancel()
		fetches := client.PollRecords(ctx, 1)
		ctxLog.Debugf("Read done for partition %d (%d-%d) at offset %d", p, pStart, pEnd, offset)
		fetches.EachError(func(topic string, partition int32, e error) {
			// In random read mode, we tolerate read errors: if the server is unavailable
			// we will just proceed to read the next random offset.
			ctxLog.Errorf("Error reading from partition %s:%d: %v", topic, partition, e)
		})
		fetches.EachRecord(func(r *kgo.Record) {
			if r.Partition != p {
				util.Die("Wrong partition %d in read at offset %d on partition %s/%d", r.Partition, r.Offset, w.config.workerCfg.Topic, p)
			}
			w.Status.Validator.ValidateRecord(r, &validRanges)
		})
		if len(fetches.Records()) == 0 {
			ctxLog.Errorf("Empty response reading from partition %d at %d", p, offset)
		} else {
			// Each read on which we get some records counts toward
			// the number of reads we were requested to do.
			i += 1
		}
		fetches = nil

		client.Flush(context.Background())
		client.Close()
	}
}
