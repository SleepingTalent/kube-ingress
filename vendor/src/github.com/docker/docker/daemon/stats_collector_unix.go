// +build !windows

package daemon

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/daemon/execdriver"
	derr "github.com/docker/docker/errors"
	"github.com/docker/docker/pkg/pubsub"
	lntypes "github.com/docker/libnetwork/types"
	"github.com/opencontainers/runc/libcontainer"
	"github.com/opencontainers/runc/libcontainer/system"
)

// newStatsCollector returns a new statsCollector that collections
// network and cgroup stats for a registered container at the specified
// interval.  The collector allows non-running containers to be added
// and will start processing stats when they are started.
func newStatsCollector(interval time.Duration) *statsCollector {
	s := &statsCollector{
		interval:            interval,
		publishers:          make(map[*Container]*pubsub.Publisher),
		clockTicksPerSecond: uint64(system.GetClockTicks()),
		bufReader:           bufio.NewReaderSize(nil, 128),
	}
	go s.run()
	return s
}

// statsCollector manages and provides container resource stats
type statsCollector struct {
	m                   sync.Mutex
	interval            time.Duration
	clockTicksPerSecond uint64
	publishers          map[*Container]*pubsub.Publisher
	bufReader           *bufio.Reader
}

// collect registers the container with the collector and adds it to
// the event loop for collection on the specified interval returning
// a channel for the subscriber to receive on.
func (s *statsCollector) collect(c *Container) chan interface{} {
	s.m.Lock()
	defer s.m.Unlock()
	publisher, exists := s.publishers[c]
	if !exists {
		publisher = pubsub.NewPublisher(100*time.Millisecond, 1024)
		s.publishers[c] = publisher
	}
	return publisher.Subscribe()
}

// stopCollection closes the channels for all subscribers and removes
// the container from metrics collection.
func (s *statsCollector) stopCollection(c *Container) {
	s.m.Lock()
	if publisher, exists := s.publishers[c]; exists {
		publisher.Close()
		delete(s.publishers, c)
	}
	s.m.Unlock()
}

// unsubscribe removes a specific subscriber from receiving updates for a container's stats.
func (s *statsCollector) unsubscribe(c *Container, ch chan interface{}) {
	s.m.Lock()
	publisher := s.publishers[c]
	if publisher != nil {
		publisher.Evict(ch)
		if publisher.Len() == 0 {
			delete(s.publishers, c)
		}
	}
	s.m.Unlock()
}

func (s *statsCollector) run() {
	type publishersPair struct {
		container *Container
		publisher *pubsub.Publisher
	}
	// we cannot determine the capacity here.
	// it will grow enough in first iteration
	var pairs []publishersPair

	for range time.Tick(s.interval) {
		// it does not make sense in the first iteration,
		// but saves allocations in further iterations
		pairs = pairs[:0]

		s.m.Lock()
		for container, publisher := range s.publishers {
			// copy pointers here to release the lock ASAP
			pairs = append(pairs, publishersPair{container, publisher})
		}
		s.m.Unlock()
		if len(pairs) == 0 {
			continue
		}

		systemUsage, err := s.getSystemCPUUsage()
		if err != nil {
			logrus.Errorf("collecting system cpu usage: %v", err)
			continue
		}

		for _, pair := range pairs {
			stats, err := pair.container.stats()
			if err != nil {
				if err != execdriver.ErrNotRunning {
					logrus.Errorf("collecting stats for %s: %v", pair.container.ID, err)
				}
				continue
			}
			stats.SystemUsage = systemUsage

			// Retrieve the nw statistics from libnetwork and inject them in the Stats
			if nwStats, err := s.getNetworkStats(pair.container); err == nil {
				stats.Interfaces = nwStats
			}
			pair.publisher.Publish(stats)
		}
	}
}

const nanoSecondsPerSecond = 1e9

// getSystemCPUUsage returns the host system's cpu usage in
// nanoseconds. An error is returned if the format of the underlying
// file does not match.
//
// Uses /proc/stat defined by POSIX. Looks for the cpu
// statistics line and then sums up the first seven fields
// provided. See `man 5 proc` for details on specific field
// information.
func (s *statsCollector) getSystemCPUUsage() (uint64, error) {
	var line string
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, err
	}
	defer func() {
		s.bufReader.Reset(nil)
		f.Close()
	}()
	s.bufReader.Reset(f)
	err = nil
	for err == nil {
		line, err = s.bufReader.ReadString('\n')
		if err != nil {
			break
		}
		parts := strings.Fields(line)
		switch parts[0] {
		case "cpu":
			if len(parts) < 8 {
				return 0, derr.ErrorCodeBadCPUFields
			}
			var totalClockTicks uint64
			for _, i := range parts[1:8] {
				v, err := strconv.ParseUint(i, 10, 64)
				if err != nil {
					return 0, derr.ErrorCodeBadCPUInt.WithArgs(i, err)
				}
				totalClockTicks += v
			}
			return (totalClockTicks * nanoSecondsPerSecond) /
				s.clockTicksPerSecond, nil
		}
	}
	return 0, derr.ErrorCodeBadStatFormat
}

func (s *statsCollector) getNetworkStats(c *Container) ([]*libcontainer.NetworkInterface, error) {
	var list []*libcontainer.NetworkInterface

	sb, err := c.daemon.netController.SandboxByID(c.NetworkSettings.SandboxID)
	if err != nil {
		return list, err
	}

	stats, err := sb.Statistics()
	if err != nil {
		return list, err
	}

	// Convert libnetwork nw stats into libcontainer nw stats
	for ifName, ifStats := range stats {
		list = append(list, convertLnNetworkStats(ifName, ifStats))
	}

	return list, nil
}

func convertLnNetworkStats(name string, stats *lntypes.InterfaceStatistics) *libcontainer.NetworkInterface {
	n := &libcontainer.NetworkInterface{Name: name}
	n.RxBytes = stats.RxBytes
	n.RxPackets = stats.RxPackets
	n.RxErrors = stats.RxErrors
	n.RxDropped = stats.RxDropped
	n.TxBytes = stats.TxBytes
	n.TxPackets = stats.TxPackets
	n.TxErrors = stats.TxErrors
	n.TxDropped = stats.TxDropped
	return n
}
