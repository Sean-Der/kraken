package engine

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/MixinNetwork/mixin/logger"
	"github.com/gofrs/uuid"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v2"
)

const (
	peerTrackClosedId          = "CLOSED"
	peerTrackConnectionTimeout = 10 * time.Second
	peerTrackReadTimeout       = 3 * time.Second
	rtpBufferSize              = 65536
	rtpClockRate               = 48000
	nackThreshold              = rtpClockRate / 4
)

type Sender struct {
	id  string
	rtp *webrtc.RTPSender
}

type NackRequest struct {
	uid  string
	cid  string
	pair *rtcp.NackPair
}

type Peer struct {
	sync.RWMutex
	rid         string
	uid         string
	cid         string
	pc          *webrtc.PeerConnection
	track       *webrtc.Track
	publishers  map[string]*Sender
	subscribers map[string]*Sender
	buffer      [rtpBufferSize]*rtp.Packet
	queue       chan *rtp.Packet
	nack        chan *NackRequest
	timestamp   uint32
	connected   chan bool
}

func (engine *Engine) BuildPeer(rid, uid string, pc *webrtc.PeerConnection) *Peer {
	cid, err := uuid.NewV4()
	if err != nil {
		panic(err)
	}
	peer := &Peer{rid: rid, uid: uid, cid: cid.String(), pc: pc}
	peer.connected = make(chan bool, 1)
	peer.queue = make(chan *rtp.Packet, 48000)
	peer.nack = make(chan *NackRequest, 48000)
	peer.publishers = make(map[string]*Sender)
	peer.subscribers = make(map[string]*Sender)
	peer.handle()
	return peer
}

func (p *Peer) id() string {
	return fmt.Sprintf("%s:%s:%s", p.rid, p.uid, p.cid)
}

func (p *Peer) Close() error {
	logger.Printf("PeerClose(%s) now\n", p.id())
	p.Lock()
	p.track = nil
	p.cid = peerTrackClosedId
	err := p.pc.Close()
	p.Unlock()
	logger.Printf("PeerClose(%s) with %v\n", p.id(), err)
	return err
}

func (peer *Peer) handle() {
	go func() {
		select {
		case <-peer.connected:
		case <-time.After(peerTrackConnectionTimeout):
			logger.Printf("HandlePeer(%s) OnTrackTimeout()\n", peer.id())
			peer.Close()
		}
	}()

	peer.pc.OnSignalingStateChange(func(state webrtc.SignalingState) {
		logger.Printf("HandlePeer(%s) OnSignalingStateChange(%s)\n", peer.id(), state)
	})
	peer.pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		logger.Printf("HandlePeer(%s) OnConnectionStateChange(%s)\n", peer.id(), state)
	})
	peer.pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		logger.Printf("HandlePeer(%s) OnICEConnectionStateChange(%s)\n", peer.id(), state)
	})
	peer.pc.OnTrack(func(rt *webrtc.Track, receiver *webrtc.RTPReceiver) {
		logger.Printf("HandlePeer(%s) OnTrack(%d, %d)\n", peer.id(), rt.PayloadType(), rt.SSRC())
		if peer.track != nil || webrtc.DefaultPayloadTypeOpus != rt.PayloadType() {
			return
		}
		peer.connected <- true

		peer.Lock()
		lt, err := peer.pc.NewTrack(rt.PayloadType(), rt.SSRC(), peer.cid, peer.uid)
		if err != nil {
			panic(err)
		}
		peer.track = lt
		peer.Unlock()

		err = peer.copyTrack(rt, lt)
		logger.Printf("HandlePeer(%s) OnTrack(%d, %d) end with %s\n", peer.id(), rt.PayloadType(), rt.SSRC(), err.Error())
		peer.Close()
	})
}

func (peer *Peer) copyTrack(src, dst *webrtc.Track) error {
	go func() error {
		for {
			pkt, err := src.ReadRTP()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			peer.queue <- pkt
		}
	}()

	for {
		timer := time.NewTimer(peerTrackReadTimeout)
		select {
		case r := <-peer.nack:
			peer.HandleNack(r)
		case pkt := <-peer.queue:
			if pkt.Timestamp > peer.timestamp || pkt.Timestamp == 0 {
				peer.timestamp = pkt.Timestamp
			}
			peer.buffer[pkt.SequenceNumber] = pkt
			dst.WriteRTP(pkt)
		case <-timer.C:
			return fmt.Errorf("peer track read timeout")
		}
		timer.Stop()
	}
}

func (peer *Peer) LoopRTCP(uid string, sender *Sender) error {
	for {
		pkts, err := sender.rtp.ReadRTCP()
		if err != nil {
			logger.Printf("LoopRTCP(%s,%s,%s) with %v\n", peer.id(), uid, sender.id, err)
			return err
		}
		for _, pkt := range pkts {
			switch pkt.(type) {
			case *rtcp.TransportLayerNack:
				nack := pkt.(*rtcp.TransportLayerNack)
				for _, pair := range nack.Nacks {
					peer.nack <- &NackRequest{uid: uid, cid: sender.id, pair: &pair}
				}
			default:
			}
		}
	}
}

func (peer *Peer) HandleNack(r *NackRequest) error {
	peer.RLock()
	sender := peer.subscribers[r.uid]
	peer.RUnlock()

	if sender == nil || sender.id != r.cid {
		return nil
	}

	for _, seq := range r.pair.PacketList() {
		pkt := peer.buffer[seq]
		if peer.timestamp >= pkt.Timestamp && peer.timestamp-pkt.Timestamp < nackThreshold ||
			peer.timestamp < pkt.Timestamp && ^uint32(0)-pkt.Timestamp+peer.timestamp < nackThreshold {
			i, err := sender.rtp.SendRTP(&pkt.Header, pkt.Payload)
			logger.Verbosef("HandleNack(%s,%s,%s,%d) with %d %v\n", peer.id(), r.uid, r.cid, seq, i, err)
		}
	}
	return nil
}
