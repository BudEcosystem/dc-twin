package lldp

import (
	"context"
	"sync"
	"time"
)

// Sender transmits one endpoint's LLDPDUs as lldpd does (src/daemon/lldpd.h): once at start,
// then every interval (tx-interval, 30 s), each with a TTL of interval × TxHold (120 s) unless the
// Info names its own; and, on Stop, a shutdown frame with a TTL of 0. It sends nothing while info
// says not now (the endpoint powered off or its port without carrier), so its neighbour ages it
// out after the TTL, as it would a device that lost power.
type Sender struct {
	interval time.Duration
	write    func([]byte) error
	info     func() (Info, bool)
	// ticker makes the transmit clock (tests replace it).
	ticker func(time.Duration) (<-chan time.Time, func())

	mu      sync.Mutex
	last    *Info // the last Info sent with a TTL, for the shutdown frame
	errs    int
	trigger chan struct{}
	stop    chan struct{}
	stopped chan struct{}
	once    sync.Once
}

// NewSender makes a sender. interval is the transmit interval, 30 s when 0; write sends one
// Ethernet frame on the endpoint's port; info is what to send, false meaning not now.
func NewSender(interval time.Duration, write func([]byte) error, info func() (Info, bool)) *Sender {
	if interval <= 0 {
		interval = DefaultInterval * time.Second
	}
	return &Sender{interval: interval, write: write, info: info,
		ticker: func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTicker(d)
			return t.C, t.Stop
		},
		trigger: make(chan struct{}, 1), stop: make(chan struct{}), stopped: make(chan struct{})}
}

// TTL is the TTL the sender puts on frames whose Info has none: interval × TxHold in seconds,
// at least 1, at most 65535.
func (s *Sender) TTL() uint16 {
	ttl := int64(s.interval/time.Second) * TxHold
	switch {
	case ttl < 1:
		return 1
	case ttl > 0xffff:
		return 0xffff
	}
	return uint16(ttl)
}

// Run sends until ctx ends or Stop is called. It returns when it is done; after Stop it has
// sent the shutdown frame.
func (s *Sender) Run(ctx context.Context) {
	defer close(s.stopped)
	tick, stopTick := s.ticker(s.interval)
	defer stopTick()
	s.send()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			s.shutdown()
			return
		case <-tick:
			s.send()
		case <-s.trigger:
			s.send()
		}
	}
}

// Trigger sends now, outside the interval: as lldpd does when a port comes up or the local
// information changes (a power-on, a hostname set).
func (s *Sender) Trigger() {
	select {
	case s.trigger <- struct{}{}:
	default:
	}
}

// Stop ends Run with a shutdown frame; Done is closed once Run has returned. Calling it more
// than once is safe; called before Run, Run sends its first frame, then the shutdown, and
// returns.
func (s *Sender) Stop() {
	s.once.Do(func() { close(s.stop) })
}

// Done is closed when Run has returned.
func (s *Sender) Done() <-chan struct{} { return s.stopped }

// Errors is how many writes have failed.
func (s *Sender) Errors() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.errs
}

func (s *Sender) send() {
	in, ok := s.info()
	if !ok {
		return
	}
	if in.TTL == 0 {
		in.TTL = s.TTL()
	}
	err := s.write(Frame(nil, in))
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.errs++
		return
	}
	s.last = &in
}

// shutdown sends the shutdown frame for what was last sent, when the endpoint can still send
// (a powered-off endpoint sends nothing).
func (s *Sender) shutdown() {
	s.mu.Lock()
	last := s.last
	s.mu.Unlock()
	if last == nil {
		return
	}
	cur, ok := s.info()
	if !ok {
		return
	}
	if cur.ChassisMAC == nil && cur.ChassisID == "" {
		cur = *last
	}
	if err := s.write(Shutdown(nil, cur)); err != nil {
		s.mu.Lock()
		s.errs++
		s.mu.Unlock()
	}
}
