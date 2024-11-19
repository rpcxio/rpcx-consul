package client

import (
	"strings"
	"sync"
	"time"

	"github.com/rpcxio/libkv"
	"github.com/rpcxio/libkv/store"
	"github.com/rpcxio/libkv/store/consul"
	"github.com/smallnest/rpcx/client"
	"github.com/smallnest/rpcx/log"
)

func init() {
	consul.Register()
}

// ConsulDiscovery is a consul service discovery.
// It always returns the registered servers in consul.
type ConsulDiscovery struct {
	basePath string
	kv       store.Store
	pairsMu  sync.RWMutex
	pairs    []*client.KVPair
	chans    []chan []*client.KVPair
	mu       sync.Mutex
	// -1 means it always retry to watch until zookeeper is ok, 0 means no retry.
	RetriesAfterWatchFailed int

	filter client.ServiceDiscoveryFilter

	stopCh chan struct{}
}

// NewConsulDiscovery returns a new ConsulDiscovery.
func NewConsulDiscovery(basePath, servicePath string, consulAddr []string, options *store.Config) (*ConsulDiscovery, error) {
	kv, err := libkv.NewStore(store.CONSUL, consulAddr, options)
	if err != nil {
		log.Infof("cannot create store: %v", err)
		return nil, err
	}

	return NewConsulDiscoveryStore(basePath+"/"+servicePath, kv)
}

// NewConsulDiscoveryStore returns a new ConsulDiscovery with specified store.
func NewConsulDiscoveryStore(basePath string, kv store.Store) (*ConsulDiscovery, error) {
	if basePath[0] == '/' {
		basePath = basePath[1:]
	}

	if len(basePath) > 1 && strings.HasSuffix(basePath, "/") {
		basePath = basePath[:len(basePath)-1]
	}

	d := &ConsulDiscovery{basePath: basePath, kv: kv}
	d.stopCh = make(chan struct{})

	ps, err := kv.List(basePath)
	if err != nil && err != store.ErrKeyNotFound {
		log.Infof("cannot get services of from registry: %v, err: %v", basePath, err)
		return nil, err
	}

	pairs := make([]*client.KVPair, 0, len(ps))
	prefix := d.basePath + "/"
	for _, p := range ps {
		if !strings.HasPrefix(p.Key, prefix) { // avoid prefix issue of consul List
			continue
		}
		k := strings.TrimPrefix(p.Key, prefix)
		pair := &client.KVPair{Key: k, Value: string(p.Value)}
		if d.filter != nil && !d.filter(pair) {
			continue
		}
		pairs = append(pairs, pair)
	}
	d.pairsMu.Lock()
	d.pairs = pairs
	d.pairsMu.Unlock()
	d.RetriesAfterWatchFailed = -1
	go d.watch()
	return d, nil
}

// NewConsulDiscoveryTemplate returns a new ConsulDiscovery template.
func NewConsulDiscoveryTemplate(basePath string, consulAddr []string, options *store.Config) (*ConsulDiscovery, error) {
	if basePath[0] == '/' {
		basePath = basePath[1:]
	}

	if len(basePath) > 1 && strings.HasSuffix(basePath, "/") {
		basePath = basePath[:len(basePath)-1]
	}

	kv, err := libkv.NewStore(store.CONSUL, consulAddr, options)
	if err != nil {
		log.Infof("cannot create store: %v", err)
		return nil, err
	}

	return NewConsulDiscoveryStore(basePath, kv)
}

// Clone clones this ServiceDiscovery with new servicePath.
func (d *ConsulDiscovery) Clone(servicePath string) (client.ServiceDiscovery, error) {
	return NewConsulDiscoveryStore(d.basePath+"/"+servicePath, d.kv)
}

// SetFilter sets the filer.
func (d *ConsulDiscovery) SetFilter(filter client.ServiceDiscoveryFilter) {
	d.filter = filter
}

// GetServices returns the servers
func (d *ConsulDiscovery) GetServices() []*client.KVPair {
	d.pairsMu.RLock()
	defer d.pairsMu.RUnlock()
	return d.pairs
}

// WatchService returns a nil chan.
func (d *ConsulDiscovery) WatchService() chan []*client.KVPair {
	d.mu.Lock()
	defer d.mu.Unlock()

	ch := make(chan []*client.KVPair, 10)
	d.chans = append(d.chans, ch)
	return ch
}

func (d *ConsulDiscovery) RemoveWatcher(ch chan []*client.KVPair) {
	d.mu.Lock()
	defer d.mu.Unlock()

	var chans []chan []*client.KVPair
	for _, c := range d.chans {
		if c == ch {
			continue
		}

		chans = append(chans, c)
	}

	d.chans = chans
}

func (d *ConsulDiscovery) watch() {
	defer func() {
		d.kv.Close()
	}()
	for {
		var err error
		var c <-chan []*store.KVPair
		var tempDelay time.Duration

		retry := d.RetriesAfterWatchFailed
		for d.RetriesAfterWatchFailed < 0 || retry >= 0 {
			c, err = d.kv.WatchTree(d.basePath, d.stopCh)
			if err != nil {
				if d.RetriesAfterWatchFailed > 0 {
					retry--
				}
				if tempDelay == 0 {
					tempDelay = 1 * time.Second
				} else {
					tempDelay *= 2
				}
				if max := 30 * time.Second; tempDelay > max {
					tempDelay = max
				}
				log.Warnf("can not watchtree (with retry %d, sleep %v): %s: %v", retry, tempDelay, d.basePath, err)
				time.Sleep(tempDelay)
				continue
			}
			break
		}

		if err != nil {
			log.Errorf("can't watch %s: %v", d.basePath, err)
			return
		}

		prefix := d.basePath + "/"

	readChanges:
		for {
			select {
			case <-d.stopCh:
				log.Info("discovery has been closed")
				return
			case ps, ok := <-c:
				if !ok {
					break readChanges
				}
				var pairs []*client.KVPair // latest servers
				if ps == nil {
					d.pairsMu.Lock()
					d.pairs = pairs
					d.pairsMu.Unlock()
					continue
				}
				for _, p := range ps {
					if !strings.HasPrefix(p.Key, prefix) { // avoid prefix issue of consul List
						continue
					}
					k := strings.TrimPrefix(p.Key, prefix)
					pair := &client.KVPair{Key: k, Value: string(p.Value)}
					if d.filter != nil && !d.filter(pair) {
						continue
					}
					pairs = append(pairs, pair)
				}
				d.pairsMu.Lock()
				d.pairs = pairs
				d.pairsMu.Unlock()

				d.mu.Lock()
				for _, ch := range d.chans {
					ch := ch
					go func() {
						defer func() {
							recover()
						}()
						timer := time.NewTimer(time.Minute)
						select {
						case ch <- pairs:
						case <-timer.C:
							log.Warn("chan is full and new change has been dropped")
						}
						timer.Stop()
					}()
				}
				d.mu.Unlock()
			}
		}

		log.Warn("chan is closed and will rewatch")
	}
}

func (d *ConsulDiscovery) Close() {
	close(d.stopCh)
}
