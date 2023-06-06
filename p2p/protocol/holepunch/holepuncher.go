package holepunch

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/protocol/holepunch/pb"
	"github.com/libp2p/go-libp2p/p2p/protocol/identify"
	"github.com/libp2p/go-msgio/pbio"
	ma "github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
)

//go:generate protoc --proto_path=$PWD:$PWD/../../.. --go_out=. --go_opt=Mpb/holepunch.proto=./pb pb/holepunch.proto

// ErrHolePunchActive is returned from DirectConnect when another hole punching attempt is currently running
var ErrHolePunchActive = errors.New("another hole punching attempt to this peer is active")

const (
	dialTimeout = 2 * time.Second
	maxRetries  = 200
)


// The holePuncher is run on the peer that's behind a NAT / Firewall.
// It observes new incoming connections via a relay that it has a reservation with,
// and initiates the DCUtR protocol with them.
// It then first tries to establish a direct connection, and if that fails, it
// initiates a hole punch.
type holePuncher struct {
	ctx       context.Context
	ctxCancel context.CancelFunc

	host     host.Host
	refCount sync.WaitGroup

	ids identify.IDService

	// active hole punches for deduplicating
	activeMx sync.Mutex
	active   map[peer.ID]struct{}

	closeMx sync.RWMutex
	closed  bool

	tracer *tracer
	filter AddrFilter
}

func newHolePuncher(h host.Host, ids identify.IDService, tracer *tracer, filter AddrFilter) *holePuncher {
	hp := &holePuncher{
		host:   h,
		ids:    ids,
		active: make(map[peer.ID]struct{}),
		tracer: tracer,
		filter: filter,
	}
	hp.ctx, hp.ctxCancel = context.WithCancel(context.Background())
	h.Network().Notify((*netNotifiee)(hp))
	return hp
}

func (hp *holePuncher) beginDirectConnect(p peer.ID) error {
	hp.closeMx.RLock()
	defer hp.closeMx.RUnlock()
	if hp.closed {
		return ErrClosed
	}

	hp.activeMx.Lock()
	defer hp.activeMx.Unlock()
	if _, ok := hp.active[p]; ok {
		return ErrHolePunchActive
	}

	hp.active[p] = struct{}{}
	return nil
}

// DirectConnect attempts to make a direct connection with a remote peer.
// It first attempts a direct dial (if we have a public address of that peer), and then
// coordinates a hole punch over the given relay connection.
func (hp *holePuncher) DirectConnect(p peer.ID) error {
	if err := hp.beginDirectConnect(p); err != nil {
		return err
	}

	defer func() {
		hp.activeMx.Lock()
		delete(hp.active, p)
		hp.activeMx.Unlock()
	}()

	return hp.directConnect(p)
}

func (hp *holePuncher) directConnect(rp peer.ID) error {
	// short-circuit check to see if we already have a direct connection
	if getDirectConnection(hp.host, rp) != nil {
		return nil
	}

	// short-circuit hole punching if a direct dial works.
	// attempt a direct connection ONLY if we have a public address for the remote peer
	for _, a := range hp.host.Peerstore().Addrs(rp) {
		if manet.IsPublicAddr(a) && !isRelayAddress(a) {
			forceDirectConnCtx := network.WithForceDirectDial(hp.ctx, "hole-punching")
			dialCtx, cancel := context.WithTimeout(forceDirectConnCtx, dialTimeout)

			tstart := time.Now()
			// This dials *all* public addresses from the peerstore.
			err := hp.host.Connect(dialCtx, peer.AddrInfo{ID: rp})
			dt := time.Since(tstart)
			cancel()

			if err != nil {
				hp.tracer.DirectDialFailed(rp, dt, err)
				break
			}
			hp.tracer.DirectDialSuccessful(rp, dt)
			log.Debugw("direct connection to peer successful, no need for a hole punch", "peer", rp)
			return nil
		}
	}

	log.Debugw("got inbound proxy conn", "peer", rp)

	// hole punch
	for i := 1; i <= maxRetries; i++ {
		addrs, obsAddrs, rtt, err := hp.initiateHolePunch(rp)
		if err != nil {
			log.Debugw("hole punching failed", "peer", rp, "error", err)
			hp.tracer.ProtocolError(rp, err)
			return err
		}
		synTime := rtt / 2
		log.Debugf("peer RTT is %s; starting hole punch in %s", rtt, synTime)

		// wait for sync to reach the other peer and then punch a hole for it in our NAT
		// by attempting a connect to it.
		timer := time.NewTimer(synTime)
		select {
		case start := <-timer.C:
			pi := peer.AddrInfo{
				ID:    rp,
				Addrs: addrs,
			}
			hp.tracer.StartHolePunch(rp, addrs, rtt)
			hp.tracer.HolePunchAttempt(pi.ID)
			err := holePunchConnect(hp.ctx, hp.host, pi, true)
			dt := time.Since(start)
			hp.tracer.EndHolePunch(rp, dt, err)
			if err == nil {
				log.Debugw("hole punching with successful", "peer", rp, "time", dt)
				hp.tracer.HolePunchFinished("initiator", i, addrs, obsAddrs, getDirectConnection(hp.host, rp))
				return nil
			}
		case <-hp.ctx.Done():
			timer.Stop()
			return hp.ctx.Err()
		}
		if i == maxRetries {
			hp.tracer.HolePunchFinished("initiator", maxRetries, addrs, obsAddrs, nil)
		}
	}
	return fmt.Errorf("all retries for hole punch with peer %s failed", rp)
}

// initiateHolePunch opens a new hole punching coordination stream,
// exchanges the addresses and measures the RTT.
func (hp *holePuncher) initiateHolePunch(rp peer.ID) ([]ma.Multiaddr, []ma.Multiaddr, time.Duration, error) {
	hpCtx := network.WithUseTransient(hp.ctx, "hole-punch")
	sCtx := network.WithNoDial(hpCtx, "hole-punch")

	str, err := hp.host.NewStream(sCtx, rp, Protocol)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("failed to open hole-punching stream: %w", err)
	}
	defer str.Close()

	addr, obsAddr, rtt, err := hp.initiateHolePunchImpl(str)
	if err != nil {
		log.Debugf("%s", err)
		str.Reset()
		return addr, obsAddr, rtt, err
	}
	return addr, obsAddr, rtt, err
}

func (hp *holePuncher) initiateHolePunchImpl(str network.Stream) ([]ma.Multiaddr, []ma.Multiaddr, time.Duration, error) {
	if err := str.Scope().SetService(ServiceName); err != nil {
		return nil, nil, 0, fmt.Errorf("error attaching stream to holepunch service: %s", err)
	}

	if err := str.Scope().ReserveMemory(maxMsgSize, network.ReservationPriorityAlways); err != nil {
		return nil, nil, 0, fmt.Errorf("error reserving memory for stream: %s", err)
	}
	defer str.Scope().ReleaseMemory(maxMsgSize)

	w := pbio.NewDelimitedWriter(str)
	rd := pbio.NewDelimitedReader(str, maxMsgSize)

	str.SetDeadline(time.Now().Add(StreamTimeout))

	// send a CONNECT and start RTT measurement.
	obsAddrs := removeRelayAddrs(hp.ids.OwnObservedAddrs())
	if hp.filter != nil {
		obsAddrs = hp.filter.FilterLocal(str.Conn().RemotePeer(), obsAddrs)
	}
	if len(obsAddrs) == 0 {
		return nil, nil, 0, errors.New("aborting hole punch initiation as we have no public address")
	}

	start := time.Now()
	if err := w.WriteMsg(&pb.HolePunch{
		Type:     pb.HolePunch_CONNECT.Enum(),
		ObsAddrs: addrsToBytes(obsAddrs),
	}); err != nil {
		str.Reset()
		return nil, nil, 0, err
	}

	// wait for a CONNECT message from the remote peer
	var msg pb.HolePunch
	if err := rd.ReadMsg(&msg); err != nil {
		return nil, nil, 0, fmt.Errorf("failed to read CONNECT message from remote peer: %w", err)
	}
	rtt := time.Since(start)
	if t := msg.GetType(); t != pb.HolePunch_CONNECT {
		return nil, nil, 0, fmt.Errorf("expect CONNECT message, got %s", t)
	}

	addrs := removeRelayAddrs(addrsFromBytes(msg.ObsAddrs))
	if hp.filter != nil {
		addrs = hp.filter.FilterRemote(str.Conn().RemotePeer(), addrs)
	}

	if len(addrs) == 0 {
		return nil, nil, 0, errors.New("didn't receive any public addresses in CONNECT")
	}

	if err := w.WriteMsg(&pb.HolePunch{Type: pb.HolePunch_SYNC.Enum()}); err != nil {
		return nil, nil, 0, fmt.Errorf("failed to send SYNC message for hole punching: %w", err)
	}
	return addrs, obsAddrs, rtt, nil
}

func (hp *holePuncher) Close() error {
	hp.closeMx.Lock()
	hp.closed = true
	hp.closeMx.Unlock()
	hp.ctxCancel()
	hp.refCount.Wait()
	return nil
}

type netNotifiee holePuncher

func (nn *netNotifiee) Connected(_ network.Network, conn network.Conn) {
	hs := (*holePuncher)(nn)

	// Hole punch if it's an inbound proxy connection.
	// If we already have a direct connection with the remote peer, this will be a no-op.
	if conn.Stat().Direction == network.DirInbound && isRelayAddress(conn.RemoteMultiaddr()) {
		hs.refCount.Add(1)
		go func() {
			defer hs.refCount.Done()

			select {
			// waiting for Identify here will allow us to access the peer's public and observed addresses
			// that we can dial to for a hole punch.
			case <-hs.ids.IdentifyWait(conn):
			case <-hs.ctx.Done():
				return
			}

			_ = hs.DirectConnect(conn.RemotePeer())
		}()
	}
}

func (nn *netNotifiee) Disconnected(_ network.Network, v network.Conn) {}
func (nn *netNotifiee) Listen(n network.Network, a ma.Multiaddr)       {}
func (nn *netNotifiee) ListenClose(n network.Network, a ma.Multiaddr)  {}
