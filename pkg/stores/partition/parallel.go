package partition

import (
	"context"
	"encoding/base64"
	"encoding/json"

	"github.com/rancher/apiserver/pkg/types"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
)

// Partition represents a named grouping of kubernetes resources,
// such as by namespace or a set of names.
type Partition interface {
	Name() string
}

// ParallelPartitionLister defines how a set of partitions will be queried.
type ParallelPartitionLister struct {
	// Lister is the lister method for a single partition.
	Lister PartitionLister

	// Concurrency is the weight of the semaphore.
	Concurrency int64

	// Partitions is the set of partitions that will be concurrently queried.
	Partitions []Partition

	state    *listState
	revision string
	err      error
}

// PartitionLister lists objects for one partition.
type PartitionLister func(ctx context.Context, partition Partition, cont string, revision string, limit int) (types.APIObjectList, error)

// Err returns the latest error encountered.
func (p *ParallelPartitionLister) Err() error {
	return p.err
}

// Revision returns the revision for the current list state.
func (p *ParallelPartitionLister) Revision() string {
	return p.revision
}

// Continue returns the encoded continue token based on the current list state.
func (p *ParallelPartitionLister) Continue() string {
	if p.state == nil {
		return ""
	}
	bytes, err := json.Marshal(p.state)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(bytes)
}

func indexOrZero(partitions []Partition, name string) int {
	if name == "" {
		return 0
	}
	for i, partition := range partitions {
		if partition.Name() == name {
			return i
		}
	}
	return 0
}

// List returns a stream of objects up to the requested limit.
// If the continue token is not empty, it decodes it and returns the stream
// starting at the indicated marker.
func (p *ParallelPartitionLister) List(ctx context.Context, limit int, resume string) (<-chan []types.APIObject, error) {
	var state listState
	if resume != "" {
		bytes, err := base64.StdEncoding.DecodeString(resume)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(bytes, &state); err != nil {
			return nil, err
		}

		if state.Limit > 0 {
			limit = state.Limit
		}
	}

	result := make(chan []types.APIObject)
	go p.feeder(ctx, state, limit, result)
	return result, nil
}

// listState is a representation of the continuation point for a partial list.
// It is encoded as the continue token in the returned response.
type listState struct {
	// Revision is the resourceVersion for the List object.
	Revision string `json:"r,omitempty"`

	// PartitionName is the name of the partition.
	PartitionName string `json:"p,omitempty"`

	// Continue is the continue token returned from Kubernetes for a partially filled list request.
	// It is a subfield of the continue token returned from steve.
	Continue string `json:"c,omitempty"`

	// Offset is the offset from the start of the list within the partition to begin the result list.
	Offset int `json:"o,omitempty"`

	// Limit is the maximum number of items from all partitions to return in the result.
	Limit int `json:"l,omitempty"`
}

// feeder spawns a goroutine to list resources in each partition and feeds the
// results, in order by partition index, into a channel.
// If the sum of the results from all partitions (by namespaces or names) is
// greater than the limit parameter from the user request or the default of
// 100000, the result is truncated and a continue token is generated that
// indicates the partition and offset for the client to start on in the next
// request.
func (p *ParallelPartitionLister) feeder(ctx context.Context, state listState, limit int, result chan []types.APIObject) {
	var (
		sem      = semaphore.NewWeighted(p.Concurrency)
		capacity = limit
		last     chan struct{}
	)

	eg, ctx := errgroup.WithContext(ctx)
	defer func() {
		err := eg.Wait()
		if p.err == nil {
			p.err = err
		}
		close(result)
	}()

	for i := indexOrZero(p.Partitions, state.PartitionName); i < len(p.Partitions); i++ {
		if capacity <= 0 || isDone(ctx) {
			break
		}

		var (
			partition = p.Partitions[i]
			tickets   = int64(1)
			turn      = last
			next      = make(chan struct{})
		)

		// setup a linked list of channel to control insertion order
		last = next

		// state.Revision is decoded from the continue token, there won't be a revision on the first request.
		if state.Revision == "" {
			// don't have a revision yet so grab all tickets to set a revision
			tickets = 3
		}
		if err := sem.Acquire(ctx, tickets); err != nil {
			p.err = err
			break
		}

		// make state local for this partition
		state := state
		eg.Go(func() error {
			defer sem.Release(tickets)
			defer close(next)

			for {
				cont := ""
				if partition.Name() == state.PartitionName {
					cont = state.Continue
				}
				list, err := p.Lister(ctx, partition, cont, state.Revision, limit)
				if err != nil {
					return err
				}

				waitForTurn(ctx, turn)
				if p.state != nil {
					return nil
				}

				if state.Revision == "" {
					state.Revision = list.Revision
				}

				if p.revision == "" {
					p.revision = list.Revision
				}

				// We have already seen the first objects in the list, truncate up to the offset.
				if state.PartitionName == partition.Name() && state.Offset > 0 && state.Offset < len(list.Objects) {
					list.Objects = list.Objects[state.Offset:]
				}

				// Case 1: the capacity has been reached across all goroutines but the list is still only partial,
				// so save the state so that the next page can be requested later.
				if len(list.Objects) > capacity {
					result <- list.Objects[:capacity]
					// save state to redo this list at this offset
					p.state = &listState{
						Revision:      list.Revision,
						PartitionName: partition.Name(),
						Continue:      cont,
						Offset:        capacity,
						Limit:         limit,
					}
					capacity = 0
					return nil
				}
				result <- list.Objects
				capacity -= len(list.Objects)
				// Case 2: all objects have been returned, we are done.
				if list.Continue == "" {
					return nil
				}
				// Case 3: we started at an offset and truncated the list to skip the objects up to the offset.
				// We're not yet up to capacity and have not retrieved every object,
				// so loop again and get more data.
				state.Continue = list.Continue
				state.PartitionName = partition.Name()
				state.Offset = 0
			}
		})
	}

	p.err = eg.Wait()
}

func waitForTurn(ctx context.Context, turn chan struct{}) {
	if turn == nil {
		return
	}
	select {
	case <-turn:
	case <-ctx.Done():
	}
}

func isDone(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}
