package goka

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/Shopify/sarama"
	"github.com/lovoo/goka/logger"
	"github.com/lovoo/goka/multierr"
	"github.com/lovoo/goka/storage"
)

const (
	// ViewStateIdle  - the view is not started yet
	ViewStateIdle State = iota
	// ViewStateCatchUp - the view is still catching up
	ViewStateCatchUp
	// ViewStateRunning - the view has caught up and is running
	ViewStateRunning
)

// Getter functions return a value for a key or an error. If no value exists for the key, nil is returned without errors.
type Getter func(string) (interface{}, error)

// View is a materialized (i.e. persistent) cache of a group table.
type View struct {
	brokers    []string
	topic      string
	opts       *voptions
	log        logger.Logger
	partitions []*PartitionTable
	consumer   sarama.Consumer
	tmgr       TopicManager
	state      *Signal
}

// NewView creates a new View object from a group.
func NewView(brokers []string, topic Table, codec Codec, options ...ViewOption) (*View, error) {
	options = append(
		// default options comes first
		[]ViewOption{
			WithViewLogger(logger.Default()),
			WithViewCallback(DefaultUpdate),
			WithViewStorageBuilder(storage.DefaultBuilder(DefaultViewStoragePath())),
		},

		// then the user passed options
		options...,
	)

	opts := new(voptions)
	err := opts.applyOptions(topic, codec, options...)
	if err != nil {
		return nil, fmt.Errorf("Error applying user-defined options: %v", err)
	}

	consumer, err := opts.builders.consumerSarama(brokers, opts.clientID)
	if err != nil {
		return nil, fmt.Errorf("Error creating sarama consumer for brokers %+v: %v", brokers, err)
	}
	opts.tableCodec = codec

	tmgr, err := opts.builders.topicmgr(brokers)
	if err != nil {
		return nil, fmt.Errorf("Error creating topic manager: %v", err)
	}

	v := &View{
		brokers:  brokers,
		topic:    string(topic),
		opts:     opts,
		log:      opts.log.Prefix(fmt.Sprintf("View %s", topic)),
		consumer: consumer,
		tmgr:     tmgr,
		state:    NewSignal(ViewStateIdle, ViewStateCatchUp, ViewStateRunning).SetState(ViewStateIdle),
	}

	if err = v.createPartitions(brokers); err != nil {
		return nil, err
	}

	return v, err
}

// WaitRunning returns a channel that will be closed when the view enters the running state
func (v *View) WaitRunning() <-chan struct{} {
	return v.state.WaitForState(ViewStateRunning)
}

func (v *View) createPartitions(brokers []string) (rerr error) {
	tm, err := v.opts.builders.topicmgr(brokers)
	if err != nil {
		return fmt.Errorf("Error creating topic manager: %v", err)
	}
	defer func() {
		e := tm.Close()
		if e != nil && rerr == nil {
			rerr = fmt.Errorf("Error closing topic manager: %v", e)
		}
	}()

	partitions, err := tm.Partitions(v.topic)
	if err != nil {
		return fmt.Errorf("Error getting partitions for topic %s: %v", v.topic, err)
	}

	// check assumption that partitions are gap-less
	for i, p := range partitions {
		if i != int(p) {
			return fmt.Errorf("Partition numbers are not sequential for topic %s", v.topic)
		}
	}

	for partID, p := range partitions {
		v.partitions = append(v.partitions, newPartitionTable(v.topic,
			p,
			v.consumer,
			v.tmgr,
			v.opts.updateCallback,
			v.opts.builders.storage,
			v.log.Prefix(fmt.Sprintf("PartTable-%d", partID)),
		))
	}

	return nil
}

// Run starts consuming the view's topic.
func (v *View) Run(ctx context.Context) (rerr error) {
	v.log.Printf("starting")
	defer v.log.Printf("stopped")

	v.state.SetState(ViewStateCatchUp)

	errg, ctx := multierr.NewErrGroup(ctx)

	multiWait := multierr.NewMultiWait(ctx, len(v.partitions))
	go func() {
		if multiWait.Wait() {
			v.state.SetState(ViewStateRunning)
		}
	}()

	for _, partition := range v.partitions {
		partition := partition
		errg.Go(func() error {
			catchupChan, errChan := partition.SetupAndCatchupForever(ctx, v.opts.restartable)

			multiWait.Add(catchupChan)

			select {
			case <-ctx.Done():
				return nil
			case err, ok := <-errChan:
				if ok && err != nil {
					return fmt.Errorf("Error while setup/catching up/recovering: %v", err)
				}
			}
			return nil
		})
	}

	// close the view after running
	defer func() {
		errs := new(multierr.Errors)
		errs.Collect(rerr)
		errs.Collect(v.close())
		rerr = errs.NilOrError()
	}()

	// wait for all partitions to finish up (or be terminated)
	rerr = errg.Wait().NilOrError()
	return
}

// close closes all storage partitions
func (v *View) close() error {
	errg, _ := multierr.NewErrGroup(context.Background())
	for _, p := range v.partitions {
		p := p
		errg.Go(func() error {
			return p.Close()
		})
	}
	v.partitions = nil
	return errg.Wait().NilOrError()
}

func (v *View) hash(key string) (int32, error) {
	// create a new hasher every time. Alternative would be to store the hash in
	// view and every time reset the hasher (ie, hasher.Reset()). But that would
	// also require us to protect the access of the hasher with a mutex.
	hasher := v.opts.hasher()

	_, err := hasher.Write([]byte(key))
	if err != nil {
		return -1, err
	}
	hash := int32(hasher.Sum32())
	if hash < 0 {
		hash = -hash
	}
	if len(v.partitions) == 0 {
		return 0, errors.New("no partitions found")
	}
	return hash % int32(len(v.partitions)), nil
}

func (v *View) find(key string) (storage.Storage, error) {
	h, err := v.hash(key)
	if err != nil {
		return nil, err
	}
	return v.partitions[h].st, nil
}

// Topic returns  the view's topic
func (v *View) Topic() string {
	return v.topic
}

// Get returns the value for the key in the view, if exists. Nil if it doesn't.
// Get can be called by multiple goroutines concurrently.
// Get can only be called after Recovered returns true.
func (v *View) Get(key string) (interface{}, error) {
	// find partition where key is located
	s, err := v.find(key)
	if err != nil {
		return nil, err
	}

	// get key and return
	data, err := s.Get(key)
	if err != nil {
		return nil, fmt.Errorf("error getting value (key %s): %v", key, err)
	} else if data == nil {
		return nil, nil
	}

	// decode value
	value, err := v.opts.tableCodec.Decode(data)
	if err != nil {
		return nil, fmt.Errorf("error decoding value (key %s): %v", key, err)
	}

	// if the key does not exist the return value is nil
	return value, nil
}

// Has checks whether a value for passed key exists in the view.
func (v *View) Has(key string) (bool, error) {
	// find partition where key is located
	s, err := v.find(key)
	if err != nil {
		return false, err
	}

	return s.Has(key)
}

// Iterator returns an iterator that iterates over the state of the View.
func (v *View) Iterator() (Iterator, error) {
	iters := make([]storage.Iterator, 0, len(v.partitions))
	for i := range v.partitions {
		iter, err := v.partitions[i].st.Iterator()
		if err != nil {
			// release already opened iterators
			for i := range iters {
				iters[i].Release()
			}

			return nil, fmt.Errorf("error opening partition iterator: %v", err)
		}

		iters = append(iters, iter)
	}

	return &iterator{
		iter:  storage.NewMultiIterator(iters),
		codec: v.opts.tableCodec,
	}, nil
}

// IteratorWithRange returns an iterator that iterates over the state of the View. This iterator is build using the range.
func (v *View) IteratorWithRange(start, limit string) (Iterator, error) {
	iters := make([]storage.Iterator, 0, len(v.partitions))
	for i := range v.partitions {
		iter, err := v.partitions[i].st.IteratorWithRange([]byte(start), []byte(limit))
		if err != nil {
			// release already opened iterators
			for i := range iters {
				iters[i].Release()
			}

			return nil, fmt.Errorf("error opening partition iterator: %v", err)
		}

		iters = append(iters, iter)
	}

	return &iterator{
		iter:  storage.NewMultiIterator(iters),
		codec: v.opts.tableCodec,
	}, nil
}

// Evict removes the given key only from the local cache. In order to delete a
// key from Kafka and other Views, context.Delete should be used on a Processor.
func (v *View) Evict(key string) error {
	s, err := v.find(key)
	if err != nil {
		return err
	}

	return s.Delete(key)
}

// Recovered returns true when the view has caught up with events from kafka.
func (v *View) Recovered() bool {
	for _, p := range v.partitions {
		if !p.IsRecovered() {
			return false
		}
	}

	return true
}

// Stats returns a set of performance metrics of the view.
func (v *View) Stats(ctx context.Context) *ViewStats {
	return v.statsWithContext(ctx)
}

func (v *View) statsWithContext(ctx context.Context) *ViewStats {
	var (
		m     sync.Mutex
		stats = newViewStats()
	)
	errg, ctx := multierr.NewErrGroup(ctx)

	for _, partTable := range v.partitions {
		partTable := partTable

		errg.Go(func() error {
			tableStats := partTable.fetchStats(ctx)
			m.Lock()
			defer m.Unlock()

			stats.Partitions[partTable.partition] = tableStats
			return nil
		})
	}

	err := errg.Wait().NilOrError()
	if err != nil {
		v.log.Printf("Error retrieving stats: %v", err)
	}
	return stats
}
