package redwood

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/pkg/errors"

	"github.com/brynbellomy/redwood/ctx"
	"github.com/brynbellomy/redwood/tree"
	"github.com/brynbellomy/redwood/types"
)

type Host interface {
	ctx.Logger
	Ctx() *ctx.Context
	Start() error

	StateAtVersion(stateURI string, version *types.ID) (tree.Node, error)
	Subscribe(ctx context.Context, stateURI string, subscriptionType SubscriptionType, keypath tree.Keypath) (ReadableSubscription, error)
	Unsubscribe(stateURI string) error
	SendTx(ctx context.Context, tx Tx) error
	AddRef(reader io.ReadCloser) (types.Hash, types.Hash, error)
	FetchRef(ctx context.Context, ref types.RefID)
	AddPeer(dialInfo PeerDialInfo)
	Transport(name string) Transport
	Controllers() ControllerHub
	Address() types.Address
	ChallengePeerIdentity(ctx context.Context, peer Peer) (SigningPublicKey, EncryptingPublicKey, error)

	ProvidersOfStateURI(ctx context.Context, stateURI string) <-chan Peer
	ProvidersOfRef(ctx context.Context, refID types.RefID) <-chan Peer
	PeersClaimingAddress(ctx context.Context, address types.Address) <-chan Peer

	HandleFetchHistoryRequest(stateURI string, fromTxID types.ID, toVersion types.ID, writeSub WritableSubscription) error
	HandleWritableSubscriptionOpened(writeSub WritableSubscription)
	HandleWritableSubscriptionClosed(writeSub WritableSubscription)
	HandleTxReceived(tx Tx, peer Peer)
	HandleAckReceived(stateURI string, txID types.ID, peer Peer)
	HandleChallengeIdentity(challengeMsg types.ChallengeMsg, peer Peer) error
	HandleFetchRefReceived(refID types.RefID, peer Peer)
}

type host struct {
	*ctx.Context

	config *Config

	signingKeypair    *SigningKeypair
	encryptingKeypair *EncryptingKeypair

	readableSubscriptions   map[string]*multiReaderSubscription // map[stateURI]
	readableSubscriptionsMu sync.RWMutex
	writableSubscriptions   map[string]map[WritableSubscription]struct{} // map[stateURI]
	writableSubscriptionsMu sync.RWMutex
	peerSeenTxs             map[PeerDialInfo]map[string]map[types.ID]bool
	peerSeenTxsMu           sync.RWMutex

	verifyPeersWorker WorkQueue

	controllerHub ControllerHub
	transports    map[string]Transport
	peerStore     PeerStore
	refStore      RefStore

	chRefsNeeded chan []types.RefID
}

var (
	ErrUnsignedTx = errors.New("unsigned tx")
	ErrProtocol   = errors.New("protocol error")
	ErrPeerIsSelf = errors.New("peer is self")
)

func NewHost(
	signingKeypair *SigningKeypair,
	encryptingKeypair *EncryptingKeypair,
	transports []Transport,
	controllerHub ControllerHub,
	refStore RefStore,
	peerStore PeerStore,
	config *Config,
) (Host, error) {
	transportsMap := make(map[string]Transport)
	for _, tpt := range transports {
		transportsMap[tpt.Name()] = tpt
	}
	h := &host{
		Context:               &ctx.Context{},
		transports:            transportsMap,
		controllerHub:         controllerHub,
		signingKeypair:        signingKeypair,
		encryptingKeypair:     encryptingKeypair,
		readableSubscriptions: make(map[string]*multiReaderSubscription),
		writableSubscriptions: make(map[string]map[WritableSubscription]struct{}),
		peerSeenTxs:           make(map[PeerDialInfo]map[string]map[types.ID]bool),
		peerStore:             peerStore,
		refStore:              refStore,
		chRefsNeeded:          make(chan []types.RefID, 100),
		config:                config,
	}
	return h, nil
}

func (h *host) Ctx() *ctx.Context {
	return h.Context
}

func (h *host) Start() error {
	return h.CtxStart(
		// on startup
		func() error {
			h.SetLogLabel("host")

			// Set up the peer store
			h.peerStore.OnNewUnverifiedPeer(h.handleNewUnverifiedPeer)
			h.verifyPeersWorker = NewWorkQueue(1, h.verifyPeers)

			// Set up the controller Hub
			h.controllerHub.OnNewState(h.handleNewState)

			h.CtxAddChild(h.controllerHub.Ctx(), nil)
			err := h.controllerHub.Start()
			if err != nil {
				return err
			}

			// Set up the ref store
			h.refStore.OnRefsNeeded(h.handleRefsNeeded)

			// Set up the transports
			for _, transport := range h.transports {
				transport.SetHost(h)
				h.CtxAddChild(transport.Ctx(), nil)
				err := transport.Start()
				if err != nil {
					return err
				}
			}

			go h.periodicallyFetchMissingRefs()
			go h.periodicallyVerifyPeers()

			return nil
		},
		nil,
		nil,
		// on shutdown
		func() {},
	)
}

func (h *host) Transport(name string) Transport {
	return h.transports[name]
}

func (h *host) Controllers() ControllerHub {
	return h.controllerHub
}

func (h *host) Address() types.Address {
	return h.signingKeypair.Address()
}

func (h *host) StateAtVersion(stateURI string, version *types.ID) (tree.Node, error) {
	return h.Controllers().StateAtVersion(stateURI, version)
}

// Returns peers discovered through any transport that have already been authenticated.
// It's not guaranteed that they actually provide
func (h *host) ProvidersOfStateURI(ctx context.Context, stateURI string) <-chan Peer {
	var wg sync.WaitGroup
	ch := make(chan Peer)
	for _, tpt := range h.transports {
		innerCh, err := tpt.ProvidersOfStateURI(ctx, stateURI)
		if err != nil {
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-h.Ctx().Done():
					return
				case <-ctx.Done():
					return
				case peer, open := <-innerCh:
					if !open {
						return
					}

					select {
					case <-h.Ctx().Done():
						return
					case <-ctx.Done():
						return
					case ch <- peer:
					}
				}
			}
		}()

	}

	go func() {
		defer close(ch)
		wg.Wait()
	}()

	return ch
}

func (h *host) ProvidersOfRef(ctx context.Context, refID types.RefID) <-chan Peer {
	var wg sync.WaitGroup
	ch := make(chan Peer)
	for _, tpt := range h.transports {
		innerCh, err := tpt.ProvidersOfRef(ctx, refID)
		if err != nil {
			h.Warnf("transport %v could not fetch providers of ref %v", tpt.Name(), refID)
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-h.Ctx().Done():
					return
				case <-ctx.Done():
					return
				case peer, open := <-innerCh:
					if !open {
						return
					}

					select {
					case <-h.Ctx().Done():
						return
					case <-ctx.Done():
						return
					case ch <- peer:
					}
				}
			}
		}()
	}

	go func() {
		defer close(ch)
		wg.Wait()
	}()

	return ch
}

func (h *host) PeersClaimingAddress(ctx context.Context, address types.Address) <-chan Peer {
	var wg sync.WaitGroup
	ch := make(chan Peer)
	for _, tpt := range h.transports {
		innerCh, err := tpt.PeersClaimingAddress(ctx, address)
		if err != nil {
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-h.Ctx().Done():
					return
				case <-ctx.Done():
					return
				case peer, open := <-innerCh:
					if !open {
						return
					}

					select {
					case <-h.Ctx().Done():
						return
					case <-ctx.Done():
						return
					case ch <- peer:
					}
				}
			}
		}()

	}

	go func() {
		defer close(ch)
		wg.Wait()
	}()

	return ch
}

func (h *host) HandleTxReceived(tx Tx, peer Peer) {
	h.Infof(0, "tx received: tx=%v peer=%v", tx.ID.Pretty(), peer.DialInfo())
	h.markTxSeenByPeer(peer, tx.StateURI, tx.ID)

	have, err := h.controllerHub.HaveTx(tx.StateURI, tx.ID)
	if err != nil {
		h.Errorf("error fetching tx %v from store: %v", tx.ID.Pretty(), err)
		// @@TODO: does it make sense to return here?
		return
	}

	if !have {
		err := h.controllerHub.AddTx(&tx, false)
		if err != nil {
			h.Errorf("error adding tx to controllerHub: %v", err)
		}
	}

	err = peer.Ack(tx.StateURI, tx.ID)
	if err != nil {
		h.Errorf("error ACKing peer: %v", err)
	}
}

func (h *host) HandleAckReceived(stateURI string, txID types.ID, peer Peer) {
	h.Infof(0, "ack received: tx=%v peer=%v", txID.Hex(), peer.DialInfo().DialAddr)
	h.markTxSeenByPeer(peer, stateURI, txID)
}

func (h *host) markTxSeenByPeer(peer Peer, stateURI string, txID types.ID) {
	h.peerSeenTxsMu.Lock()
	defer h.peerSeenTxsMu.Unlock()

	dialInfo := peer.DialInfo()

	if h.peerSeenTxs[dialInfo] == nil {
		h.peerSeenTxs[dialInfo] = make(map[string]map[types.ID]bool)
	}
	if h.peerSeenTxs[dialInfo][stateURI] == nil {
		h.peerSeenTxs[dialInfo][stateURI] = make(map[types.ID]bool)
	}
	h.peerSeenTxs[dialInfo][stateURI][txID] = true
}

func (h *host) txSeenByPeer(peer Peer, stateURI string, txID types.ID) bool {
	// @@TODO: convert to LRU cache

	if peer.Address() == (types.Address{}) {
		return false
	}

	h.peerSeenTxsMu.Lock()
	defer h.peerSeenTxsMu.Unlock()

	dialInfo := peer.DialInfo()

	if h.peerSeenTxs[dialInfo] == nil {
		return false
	} else if h.peerSeenTxs[dialInfo][stateURI] == nil {
		return false
	}
	return h.peerSeenTxs[dialInfo][stateURI][txID]
}

func (h *host) AddPeer(dialInfo PeerDialInfo) {
	h.peerStore.AddDialInfos([]PeerDialInfo{dialInfo})
	h.verifyPeersWorker.Enqueue()
}

func (h *host) periodicallyVerifyPeers() {
	for {
		h.verifyPeersWorker.Enqueue()
		time.Sleep(10 * time.Second)
	}
}

func (h *host) handleNewUnverifiedPeer(dialInfo PeerDialInfo) {
	h.verifyPeersWorker.Enqueue()
}

func (h *host) verifyPeers() {
	unverifiedPeers := h.peerStore.UnverifiedPeers()
	var wg sync.WaitGroup
	wg.Add(len(unverifiedPeers))
	for _, unverifiedPeer := range unverifiedPeers {
		unverifiedPeer := unverifiedPeer
		go func() {
			defer wg.Done()

			transport := h.Transport(unverifiedPeer.DialInfo().TransportName)
			if transport == nil {
				// Unsupported transport
				return
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			peer, err := transport.NewPeerConn(ctx, unverifiedPeer.DialInfo().DialAddr)
			if errors.Cause(err) == ErrPeerIsSelf {
				return
			} else if errors.Cause(err) == types.ErrConnection {
				return
			} else if err != nil {
				h.Warnf("could not get peer at %v %v: %v", unverifiedPeer.DialInfo().TransportName, unverifiedPeer.DialInfo().DialAddr, err)
				return
			}

			_, _, err = h.ChallengePeerIdentity(ctx, peer)
			if err != nil {
				h.Errorf("error verifying peer identity: %v ", err)
				return
			}
		}()
	}
	wg.Wait()
}

func (h *host) HandleFetchHistoryRequest(stateURI string, fromTxID types.ID, toVersion types.ID, writeSub WritableSubscription) error {
	// @@TODO: respect the input params

	iter := h.controllerHub.FetchTxs(stateURI, fromTxID)
	defer iter.Cancel()

	for {
		tx := iter.Next()
		if iter.Error() != nil {
			return iter.Error()
		} else if tx == nil {
			return nil
		}

		leaves, err := h.controllerHub.Leaves(stateURI)
		if err != nil {
			return err
		}

		isPrivate, err := h.controllerHub.IsPrivate(tx.StateURI)
		if err != nil {
			return err
		}

		if isPrivate {
			var isAllowed bool
			if peer, isPeer := writeSub.(Peer); isPeer {
				isAllowed, err = h.controllerHub.IsMember(tx.StateURI, peer.Address())
				if err != nil {
					h.Errorf("error determining if peer '%v' is a member of private state URI '%v': %v", peer.Address(), tx.StateURI, err)
					return err
				}
			} else {
				// In-process subscriptions are trusted
				isAllowed = true
			}

			if isAllowed {
				err = writeSub.WritePrivate(context.TODO(), tx, nil, leaves)
				if err != nil {
					h.Errorf("error writing tx to peer: %v", err)
					h.HandleWritableSubscriptionClosed(writeSub)
					return err
				}
			}

		} else {
			err = writeSub.Write(context.TODO(), tx, nil, leaves)
			if err != nil {
				h.Errorf("error writing tx to peer: %v", err)
				h.HandleWritableSubscriptionClosed(writeSub)
				return err
			}
		}
	}
	return nil
}

func (h *host) HandleWritableSubscriptionOpened(writeSub WritableSubscription) {
	if writeSub.Type().Includes(SubscriptionType_States) {
		// Normalize empty keypaths
		keypath := writeSub.Keypath()
		if keypath.Equals(tree.KeypathSeparator) {
			keypath = nil
		}

		// Immediately write the current state to the subscriber
		state, err := h.Controllers().StateAtVersion(writeSub.StateURI(), nil)
		if err != nil && errors.Cause(err) != ErrNoController {
			h.Errorf("error writing initial state to peer: %v", err)
			return
		} else if err == nil {
			defer state.Close()

			leaves, err := h.Controllers().Leaves(writeSub.StateURI())
			if err != nil {
				h.Errorf("error writing initial state to peer: %v", err)
				return
			}

			err = writeSub.Write(context.TODO(), nil, state.NodeAt(keypath, nil), leaves)
			if err != nil {
				h.Errorf("error writing tx to peer: %v", err)
				return
			}
		}
	}

	h.writableSubscriptionsMu.Lock()
	defer h.writableSubscriptionsMu.Unlock()

	if _, exists := h.writableSubscriptions[writeSub.StateURI()]; !exists {
		h.writableSubscriptions[writeSub.StateURI()] = make(map[WritableSubscription]struct{})
	}
	h.writableSubscriptions[writeSub.StateURI()][writeSub] = struct{}{}
}

func (h *host) HandleWritableSubscriptionClosed(writeSub WritableSubscription) {
	h.writableSubscriptionsMu.Lock()
	defer h.writableSubscriptionsMu.Unlock()

	err := writeSub.Close()
	if err != nil {
		h.Errorf("error closing writable subscription: %+v", err)
	}

	if _, exists := h.writableSubscriptions[writeSub.StateURI()]; exists {
		delete(h.writableSubscriptions[writeSub.StateURI()], writeSub)
	}
}

func (h *host) subscribe(ctx context.Context, stateURI string) error {
	h.readableSubscriptionsMu.Lock()
	defer h.readableSubscriptionsMu.Unlock()

	err := h.config.Update(func() error {
		h.config.Node.SubscribedStateURIs.Add(stateURI)
		return nil
	})
	if err != nil {
		return err
	}

	if _, exists := h.readableSubscriptions[stateURI]; !exists {
		multiSub := newMultiReaderSubscription(stateURI, h.config.Node.MaxPeersPerSubscription, h)
		go multiSub.Start()
		h.readableSubscriptions[stateURI] = multiSub

		go func() {
			defer multiSub.Close()

			select {
			case <-h.Ctx().Done():
			case <-multiSub.chDone:
			}

			h.readableSubscriptionsMu.Lock()
			defer h.readableSubscriptionsMu.Unlock()
			delete(h.readableSubscriptions, stateURI)
		}()
	}
	return nil
}

func (h *host) Subscribe(
	ctx context.Context,
	stateURI string,
	subscriptionType SubscriptionType,
	stateKeypath tree.Keypath,
) (ReadableSubscription, error) {
	err := h.subscribe(ctx, stateURI)
	if err != nil {
		return nil, err
	}

	h.writableSubscriptionsMu.Lock()
	defer h.writableSubscriptionsMu.Unlock()

	if _, exists := h.writableSubscriptions[stateURI]; !exists {
		h.writableSubscriptions[stateURI] = make(map[WritableSubscription]struct{})
	}

	so := &inProcessSubscription{
		stateURI:         stateURI,
		keypath:          stateKeypath,
		subscriptionType: subscriptionType,
		ch:               make(chan SubscriptionMsg),
		chStop:           make(chan struct{}),
	}
	h.writableSubscriptions[stateURI][so] = struct{}{}

	go func() {
		defer close(so.ch)

		select {
		case <-h.Ctx().Done():
		case <-so.chStop:
		}

		h.writableSubscriptionsMu.Lock()
		defer h.writableSubscriptionsMu.Unlock()
		delete(h.writableSubscriptions[stateURI], so)
	}()

	return so, nil
}

func (h *host) Unsubscribe(stateURI string) error {
	// @@TODO: when the Host unsubscribes, it should close the subs of any peers reading from it
	h.readableSubscriptionsMu.Lock()
	defer h.readableSubscriptionsMu.Unlock()

	err := h.config.Update(func() error {
		h.config.Node.SubscribedStateURIs.Remove(stateURI)
		return nil
	})
	if err != nil {
		return err
	}

	h.readableSubscriptions[stateURI].Close()
	delete(h.readableSubscriptions, stateURI)
	return nil
}

func (h *host) ChallengePeerIdentity(ctx context.Context, peer Peer) (_ SigningPublicKey, _ EncryptingPublicKey, err error) {
	defer withStack(&err)

	err = peer.EnsureConnected(ctx)
	if err != nil {
		return nil, nil, err
	}

	challengeMsg, err := types.GenerateChallengeMsg()
	if err != nil {
		return nil, nil, err
	}

	err = peer.ChallengeIdentity(types.ChallengeMsg(challengeMsg))
	if err != nil {
		return nil, nil, err
	}

	resp, err := peer.ReceiveChallengeIdentityResponse()
	if err != nil {
		return nil, nil, err
	}

	sigpubkey, err := RecoverSigningPubkey(types.HashBytes(challengeMsg), resp.Signature)
	if err != nil {
		return nil, nil, err
	}
	encpubkey := EncryptingPublicKeyFromBytes(resp.EncryptingPublicKey)

	h.peerStore.AddVerifiedCredentials(peer.DialInfo(), sigpubkey.Address(), sigpubkey, encpubkey)

	return sigpubkey, encpubkey, nil
}

func (h *host) HandleChallengeIdentity(challengeMsg types.ChallengeMsg, peer Peer) error {
	defer peer.Close()

	sig, err := h.signingKeypair.SignHash(types.HashBytes(challengeMsg))
	if err != nil {
		return err
	}
	return peer.RespondChallengeIdentity(ChallengeIdentityResponse{
		Signature:           sig,
		EncryptingPublicKey: h.encryptingKeypair.EncryptingPublicKey.Bytes(),
	})
}

func (h *host) handleNewState(tx *Tx, state tree.Node, leaves []types.ID) {
	state, err := state.CopyToMemory(nil, nil)
	if err != nil {
		h.Errorf("handleNewState: couldn't copy state to memory: %v", err)
		state = tree.NewMemoryNode() // give subscribers an empty state
	}

	// @@TODO: don't do this, this is stupid.  store ungossiped txs in the DB and create a
	// PeerManager that gossips them on a SleeperTask-like trigger.
	go func() {
		// If this is the genesis tx of a private state URI, ensure that we subscribe to that state URI
		// @@TODO: allow blacklisting of senders
		if tx.IsPrivate() && tx.ID == GenesisTxID && !h.config.Node.SubscribedStateURIs.Contains(tx.StateURI) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			sub, err := h.Subscribe(ctx, tx.StateURI, 0, nil)
			if err != nil {
				h.Errorf("error subscribing to state URI %v: %v", tx.StateURI, err)
			}
			sub.Close() // We don't need the in-process subscription
		}

		// Broadcast state and tx to others
		ctx, cancel := context.WithTimeout(h.Ctx(), 10*time.Second)
		defer cancel()

		var wg sync.WaitGroup
		var alreadySentPeers sync.Map

		wg.Add(2)
		go h.broadcastToWritableSubscribers(ctx, tx, state, leaves, &alreadySentPeers, &wg)
		go h.broadcastToStateURIProviders(ctx, tx, leaves, &alreadySentPeers, &wg)
	}()
}

func (h *host) broadcastToStateURIProviders(ctx context.Context, tx *Tx, leaves []types.ID, alreadySentPeers *sync.Map, wg *sync.WaitGroup) {
	defer wg.Done()

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second) // @@TODO: make configurable
	defer cancel()

	ch := make(chan Peer)
	go func() {
		defer close(ch)

		chProviders := h.ProvidersOfStateURI(ctx, tx.StateURI)
		for {
			select {
			case peer, open := <-chProviders:
				if !open {
					return
				}
				ch <- peer
			case <-ctx.Done():
				return
			case <-h.Ctx().Done():
				return
			}
		}
	}()

	for peer := range ch {
		if h.txSeenByPeer(peer, tx.StateURI, tx.ID) || tx.From == peer.Address() { // @@TODO: do we always want to avoid broadcasting when `from == peer.address`?
			continue
		}

		wg.Add(1)
		peer := peer
		go func() {
			defer wg.Done()

			_, alreadySent := alreadySentPeers.LoadOrStore(peer.DialInfo(), struct{}{})
			if alreadySent {
				return
			}

			err := peer.EnsureConnected(ctx)
			if err != nil {
				h.Errorf("error connecting to peer: %v", err)
				return
			}
			defer peer.Close()

			err = peer.Put(tx, nil, leaves)
			if err != nil {
				h.Errorf("error writing tx to peer: %v", err)
				return
			}
		}()
	}
}

func (h *host) broadcastToWritableSubscribers(
	ctx context.Context,
	tx *Tx,
	state tree.Node,
	leaves []types.ID,
	alreadySentPeers *sync.Map,
	wg *sync.WaitGroup,
) {
	defer wg.Done()

	h.writableSubscriptionsMu.RLock()
	defer h.writableSubscriptionsMu.RUnlock()

	for writeSub := range h.writableSubscriptions[tx.StateURI] {
		if peer, isPeer := writeSub.(Peer); isPeer {
			// If the subscriber wants us to send states, we never skip sending
			if h.txSeenByPeer(peer, tx.StateURI, tx.ID) && !writeSub.Type().Includes(SubscriptionType_States) {
				continue
			}
		}

		wg.Add(1)
		writeSub := writeSub
		go func() {
			defer wg.Done()

			_, alreadySent := alreadySentPeers.LoadOrStore(writeSub, struct{}{})
			if alreadySent {
				return
			}

			isPrivate, err := h.controllerHub.IsPrivate(tx.StateURI)
			if err != nil {
				h.Errorf("error determining if state URI '%v' is private: %v", tx.StateURI, err)
				return
			}

			// Drill down to the part of the state that the subscriber is interested in
			keypath := writeSub.Keypath()
			if keypath.Equals(tree.KeypathSeparator) {
				keypath = nil
			}
			state = state.NodeAt(keypath, nil)

			if isPrivate {
				var isAllowed bool
				if peer, isPeer := writeSub.(Peer); isPeer {
					isAllowed, err = h.controllerHub.IsMember(tx.StateURI, peer.Address())
					if err != nil {
						h.Errorf("error determining if peer '%v' is a member of private state URI '%v': %v", peer.Address(), tx.StateURI, err)
						return
					}
				} else {
					// In-process subscriptions are trusted
					isAllowed = true
				}

				if isAllowed {
					err = writeSub.WritePrivate(ctx, tx, state, leaves)
					if err != nil {
						h.Errorf("error writing tx to peer: %v", err)
						h.HandleWritableSubscriptionClosed(writeSub)
						return
					}
				}

			} else {
				err = writeSub.Write(ctx, tx, state, leaves)
				if err != nil {
					h.Errorf("error writing tx to peer: %v", err)
					h.HandleWritableSubscriptionClosed(writeSub)
					return
				}
			}
		}()
	}
}

func (h *host) SendTx(ctx context.Context, tx Tx) (err error) {
	h.Infof(0, "adding tx (%v) %v", tx.StateURI, tx.ID.Pretty())

	defer func() {
		if err != nil {
			return
		}
		// If we send a tx to a state URI that we're not subscribed to yet, auto-subscribe.
		if !h.config.Node.SubscribedStateURIs.Contains(tx.StateURI) {
			err := h.config.Update(func() error {
				h.config.Node.SubscribedStateURIs.Add(tx.StateURI)
				return nil
			})
			if err != nil {
				h.Errorf("error adding %v to config.Node.SubscribedStateURIs: %v", tx.StateURI, err)
			}
		}
	}()

	if tx.From == (types.Address{}) {
		tx.From = h.signingKeypair.Address()
	}

	if len(tx.Parents) == 0 && tx.ID != GenesisTxID {
		var parents []types.ID
		parents, err = h.controllerHub.Leaves(tx.StateURI)
		if err != nil {
			return err
		}
		tx.Parents = parents
	}

	if len(tx.Sig) == 0 {
		err = h.SignTx(&tx)
		if err != nil {
			return err
		}
	}

	err = h.controllerHub.AddTx(&tx, false)
	if err != nil {
		return err
	}
	return nil
}

func (h *host) SignTx(tx *Tx) error {
	var err error
	tx.Sig, err = h.signingKeypair.SignHash(tx.Hash())
	return err
}

func (h *host) AddRef(reader io.ReadCloser) (types.Hash, types.Hash, error) {
	return h.refStore.StoreObject(reader)
}

func (h *host) handleRefsNeeded(refs []types.RefID) {
	select {
	case <-h.Ctx().Done():
		return
	case h.chRefsNeeded <- refs:
	}
}

func (h *host) periodicallyFetchMissingRefs() {
	tick := time.NewTicker(10 * time.Second) // @@TODO: make configurable
	defer tick.Stop()

	for {
		select {
		case <-h.Ctx().Done():
			return

		case refs := <-h.chRefsNeeded:
			h.fetchMissingRefs(refs)

		case <-tick.C:
			refs, err := h.refStore.RefsNeeded()
			if err != nil {
				h.Errorf("error fetching list of needed refs: %v", err)
				continue
			}

			if len(refs) > 0 {
				h.fetchMissingRefs(refs)
			}
		}
	}
}

func (h *host) fetchMissingRefs(refs []types.RefID) {
	var wg sync.WaitGroup
	for _, refID := range refs {
		wg.Add(1)
		refID := refID
		go func() {
			defer wg.Done()
			h.FetchRef(h.Ctx(), refID)
		}()
	}
	wg.Wait()
}

func (h *host) FetchRef(ctx context.Context, refID types.RefID) {
	for peer := range h.ProvidersOfRef(ctx, refID) {
		err := peer.EnsureConnected(ctx)
		if err != nil {
			h.Errorf("error connecting to peer: %v", err)
			continue
		}

		err = peer.FetchRef(refID)
		if err != nil {
			h.Errorf("error writing to peer: %v", err)
			continue
		}

		// Not currently used
		_, err = peer.ReceiveRefHeader()
		if err != nil {
			h.Errorf("error reading from peer: %v", err)
			continue
		}

		pr, pw := io.Pipe()
		go func() {
			var err error
			defer func() { pw.CloseWithError(err) }()

			for {
				select {
				case <-ctx.Done():
					err = ctx.Err()
					return
				default:
				}

				pkt, err := peer.ReceiveRefPacket()
				if err != nil {
					h.Errorf("error receiving ref from peer: %v", err)
					return
				} else if pkt.End {
					return
				}

				var n int
				n, err = pw.Write(pkt.Data)
				if err != nil {
					h.Errorf("error receiving ref from peer: %v", err)
					return
				} else if n < len(pkt.Data) {
					err = io.ErrUnexpectedEOF
					return
				}
			}
		}()

		sha1Hash, sha3Hash, err := h.refStore.StoreObject(pr)
		if err != nil {
			h.Errorf("could not store ref: %v", err)
			continue
		}
		// @@TODO: check stored refHash against the one we requested

		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		h.announceRefs(ctx, []types.RefID{
			{HashAlg: types.SHA1, Hash: sha1Hash},
			{HashAlg: types.SHA3, Hash: sha3Hash},
		})
		return
	}
}

func (h *host) announceRefs(ctx context.Context, refIDs []types.RefID) {
	var wg sync.WaitGroup
	wg.Add(len(refIDs) * len(h.transports))

	for _, transport := range h.transports {
		for _, refID := range refIDs {
			transport := transport
			refID := refID

			go func() {
				defer wg.Done()

				err := transport.AnnounceRef(ctx, refID)
				if errors.Cause(err) == types.ErrUnimplemented {
					return
				} else if err != nil {
					h.Warnf("error announcing ref %v over transport %v: %v", refID, transport.Name(), err)
				}
			}()
		}
	}
	wg.Wait()
}

const (
	REF_CHUNK_SIZE = 1024 // @@TODO: tunable buffer size?
)

func (h *host) HandleFetchRefReceived(refID types.RefID, peer Peer) {
	defer peer.Close()

	objectReader, _, err := h.refStore.Object(refID)
	// @@TODO: handle the case where we don't have the ref more gracefully
	if err != nil {
		panic(err)
	}

	err = peer.SendRefHeader()
	if err != nil {
		h.Errorf("[ref server] %+v", errors.WithStack(err))
		return
	}

	buf := make([]byte, REF_CHUNK_SIZE)
	for {
		n, err := io.ReadFull(objectReader, buf)
		if err == io.EOF {
			break
		} else if err == io.ErrUnexpectedEOF {
			buf = buf[:n]
		} else if err != nil {
			h.Errorf("[ref server] %+v", err)
			return
		}

		err = peer.SendRefPacket(buf, false)
		if err != nil {
			h.Errorf("[ref server] %+v", errors.WithStack(err))
			return
		}
	}

	err = peer.SendRefPacket(nil, true)
	if err != nil {
		h.Errorf("[ref server] %+v", errors.WithStack(err))
		return
	}
}
