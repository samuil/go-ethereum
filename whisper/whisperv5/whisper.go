// Copyright 2016 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package whisperv5

import (
	"bytes"
	"crypto/ecdsa"
	crand "crypto/rand"
	"crypto/sha256"
	"fmt"
	mrand "math/rand"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/logger"
	"github.com/ethereum/go-ethereum/logger/glog"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/adapters"
	"golang.org/x/crypto/pbkdf2"
	set "gopkg.in/fatih/set.v0"
)

// Whisper represents a dark communication interface through the Ethereum
// network, using its very own P2P communication layer.
type Whisper struct {
	protocol p2p.Protocol
	filters  *Filters

	privateKeys map[string]*ecdsa.PrivateKey
	symKeys     map[string][]byte
	keyMu       sync.RWMutex

	envelopes   map[common.Hash]*Envelope        // Pool of messages currently tracked by this node
	messages    map[common.Hash]*ReceivedMessage // Pool of successfully decrypted messages, which are not expired yet
	expirations map[uint32]*set.SetNonTS         // Message expiration pool
	poolMu      sync.RWMutex                     // Mutex to sync the message and expiration pools

	peers  map[*Peer]struct{} // Set of currently active peers
	peerMu sync.RWMutex       // Mutex to sync the active peer set

	mailServer MailServer

	quit chan struct{}
	test bool
}

// New creates a Whisper client ready to communicate through the Ethereum P2P network.
// Param s should be passed if you want to implement mail server, otherwise nil.
func NewWhisper(server MailServer, localAddr []byte, na adapters.NodeAdapter, m adapters.Messenger) *Whisper {
	whisper := &Whisper{
		privateKeys: make(map[string]*ecdsa.PrivateKey),
		symKeys:     make(map[string][]byte),
		envelopes:   make(map[common.Hash]*Envelope),
		messages:    make(map[common.Hash]*ReceivedMessage),
		expirations: make(map[uint32]*set.SetNonTS),
		peers:       make(map[*Peer]struct{}),
		mailServer:  server,
		quit:        make(chan struct{}),
	}
	whisper.filters = NewFilters(whisper)
	whisper.protocol = Shh(whisper, localAddr, na, m)

	seed := time.Now().Unix()
	mrand.Seed(seed)

	return whisper
}

// Protocols returns the whisper sub-protocols ran by this particular client.
func (w *Whisper) Protocols() []p2p.Protocol {
	return []p2p.Protocol{w.protocol}
}

// Version returns the whisper sub-protocols version number.
func (w *Whisper) Version() uint {
	return w.protocol.Version
}

func (w *Whisper) getPeer(peerID []byte) (*Peer, error) {
	w.peerMu.Lock()
	defer w.peerMu.Unlock()
	for p, _ := range w.peers {
		id := p.peer.ID()
		if bytes.Equal(peerID, id[:]) {
			return p, nil
		}
	}
	return nil, fmt.Errorf("Could not find peer with ID: %x", peerID)
}

// MarkPeerTrusted marks specific peer trusted, which will allow it
// to send historic (expired) messages.
func (w *Whisper) MarkPeerTrusted(peerID []byte) error {
	p, err := w.getPeer(peerID)
	if err != nil {
		return err
	}
	p.trusted = true
	return nil
}

// NewIdentity generates a new cryptographic identity for the client, and injects
// it into the known identities for message decryption.
func (w *Whisper) NewIdentity() *ecdsa.PrivateKey {
	key, err := crypto.GenerateKey()
	if err != nil || !validatePrivateKey(key) {
		key, err = crypto.GenerateKey() // retry once
	}
	if err != nil {
		panic(err)
	}
	if !validatePrivateKey(key) {
		panic("Failed to generate valid key")
	}
	w.keyMu.Lock()
	defer w.keyMu.Unlock()
	w.privateKeys[common.ToHex(crypto.FromECDSAPub(&key.PublicKey))] = key
	return key
}

// DeleteIdentity deletes the specified key if it exists.
func (w *Whisper) DeleteIdentity(key string) {
	w.keyMu.Lock()
	defer w.keyMu.Unlock()
	delete(w.privateKeys, key)
}

// HasIdentity checks if the the whisper node is configured with the private key
// of the specified public pair.
func (w *Whisper) HasIdentity(pubKey string) bool {
	w.keyMu.RLock()
	defer w.keyMu.RUnlock()
	return w.privateKeys[pubKey] != nil
}

// GetIdentity retrieves the private key of the specified public identity.
func (w *Whisper) GetIdentity(pubKey string) *ecdsa.PrivateKey {
	w.keyMu.RLock()
	defer w.keyMu.RUnlock()
	return w.privateKeys[pubKey]
}

func (w *Whisper) GenerateSymKey(name string) error {
	const size = aesKeyLength * 2
	buf := make([]byte, size)
	buf2 := make([]byte, size)
	_, err := crand.Read(buf)
	if err != nil {
		return err
	} else if !validateSymmetricKey(buf) {
		return fmt.Errorf("error in GenerateSymKey: crypto/rand failed to generate random data")
	}

	randomize(buf2)
	if !validateSymmetricKey(buf2) {
		return fmt.Errorf("error in GenerateSymKey: math/rand failed to generate random data")
	}

	for i := 0; i < size; i++ {
		buf[i] ^= buf2[i]
	}

	key := buf[:aesKeyLength]
	salt := buf[aesKeyLength:]
	derived, err := DeriveOneTimeKey(key, salt, EnvelopeVersion)
	if err != nil {
		return err
	} else if !validateSymmetricKey(derived) {
		return fmt.Errorf("failed to derive valid key")
	}

	w.keyMu.Lock()
	defer w.keyMu.Unlock()

	if w.symKeys[name] != nil {
		return fmt.Errorf("Key with name [%s] already exists", name)
	}
	w.symKeys[name] = derived
	return nil
}

func (w *Whisper) AddSymKey(name string, key []byte) error {
	if w.HasSymKey(name) {
		return fmt.Errorf("Key with name [%s] already exists", name)
	}

	derived, err := deriveKeyMaterial(key, EnvelopeVersion)
	if err != nil {
		return err
	}

	w.keyMu.Lock()
	defer w.keyMu.Unlock()

	// double check is necessary, because deriveKeyMaterial() is slow
	if w.symKeys[name] != nil {
		return fmt.Errorf("Key with name [%s] already exists", name)
	}
	w.symKeys[name] = derived
	return nil
}

func (w *Whisper) HasSymKey(name string) bool {
	w.keyMu.RLock()
	defer w.keyMu.RUnlock()
	return w.symKeys[name] != nil
}

func (w *Whisper) DeleteSymKey(name string) {
	w.keyMu.Lock()
	defer w.keyMu.Unlock()
	delete(w.symKeys, name)
}

func (w *Whisper) GetSymKey(name string) []byte {
	w.keyMu.RLock()
	defer w.keyMu.RUnlock()
	return w.symKeys[name]
}

// Watch installs a new message handler to run in case a matching packet arrives
// from the whisper network.
func (w *Whisper) Watch(f *Filter) uint32 {
	return w.filters.Install(f)
}

func (w *Whisper) GetFilter(id uint32) *Filter {
	return w.filters.Get(id)
}

// Unwatch removes an installed message handler.
func (w *Whisper) Unwatch(id uint32) {
	w.filters.Uninstall(id)
}

// Send injects a message into the whisper send queue, to be distributed in the
// network in the coming cycles.
func (w *Whisper) Send(envelope *Envelope) error {
	return w.add(envelope)
}

// Start implements node.Service, starting the background data propagation thread
// of the Whisper protocol.
func (w *Whisper) Start(*p2p.Server) error {
	glog.V(logger.Info).Infoln("Whisper started")
	go w.update()
	return nil
}

// Stop implements node.Service, stopping the background data propagation thread
// of the Whisper protocol.
func (w *Whisper) Stop() error {
	close(w.quit)
	glog.V(logger.Info).Infoln("Whisper stopped")
	return nil
}

// add inserts a new envelope into the message pool to be distributed within the
// whisper network. It also inserts the envelope into the expiration pool at the
// appropriate time-stamp. In case of error, connection should be dropped.
func (wh *Whisper) add(envelope *Envelope) error {
	now := uint32(time.Now().Unix())
	sent := envelope.Expiry - envelope.TTL

	if sent > now {
		if sent-SynchAllowance > now {
			return fmt.Errorf("message created in the future")
		} else {
			// recalculate PoW, adjusted for the time difference, plus one second for latency
			envelope.calculatePoW(sent - now + 1)
		}
	}

	if envelope.Expiry < now {
		if envelope.Expiry+SynchAllowance*2 < now {
			return fmt.Errorf("very old message")
		} else {
			return nil // drop envelope without error
		}
	}

	if len(envelope.Data) > MaxMessageLength {
		return fmt.Errorf("huge messages are not allowed")
	}

	if len(envelope.Version) > 4 {
		return fmt.Errorf("oversized Version")
	}

	if len(envelope.AESNonce) > AESNonceMaxLength {
		// the standard AES GSM nonce size is 12,
		// but const gcmStandardNonceSize cannot be accessed directly
		return fmt.Errorf("oversized AESNonce")
	}

	if len(envelope.Salt) > saltLength {
		return fmt.Errorf("oversized Salt")
	}

	if envelope.PoW() < MinimumPoW && !wh.test {
		glog.V(logger.Debug).Infof("envelope with low PoW dropped: %f", envelope.PoW())
		return nil // drop envelope without error
	}

	hash := envelope.Hash()

	wh.poolMu.Lock()
	_, alreadyCached := wh.envelopes[hash]
	if !alreadyCached {
		wh.envelopes[hash] = envelope
		if wh.expirations[envelope.Expiry] == nil {
			wh.expirations[envelope.Expiry] = set.NewNonTS()
		}
		if !wh.expirations[envelope.Expiry].Has(hash) {
			wh.expirations[envelope.Expiry].Add(hash)
		}
	}
	wh.poolMu.Unlock()

	if alreadyCached {
		glog.V(logger.Detail).Infof("whisper envelope already cached: %x\n", envelope)
	} else {
		wh.postEvent(envelope, messagesCode) // notify the local node about the new message
		glog.V(logger.Detail).Infof("cached whisper envelope %v\n", envelope)
	}
	return nil
}

// postEvent delivers the message to the watchers.
func (w *Whisper) postEvent(envelope *Envelope, messageCode uint64) {
	// if the version of incoming message is higher than
	// currently supported version, we can not decrypt it,
	// and therefore just ignore this message
	if envelope.Ver() <= EnvelopeVersion {
		// todo: review if you need an additional thread here
		go w.filters.NotifyWatchers(envelope, messageCode)
	}
}

// update loops until the lifetime of the whisper node, updating its internal
// state by expiring stale messages from the pool.
func (w *Whisper) update() {
	// Start a ticker to check for expirations
	expire := time.NewTicker(expirationCycle)

	// Repeat updates until termination is requested
	for {
		select {
		case <-expire.C:
			w.expire()

		case <-w.quit:
			return
		}
	}
}

// expire iterates over all the expiration timestamps, removing all stale
// messages from the pools.
func (w *Whisper) expire() {
	w.poolMu.Lock()
	defer w.poolMu.Unlock()

	now := uint32(time.Now().Unix())
	for then, hashSet := range w.expirations {
		// Short circuit if a future time
		if then > now {
			continue
		}
		// Dump all expired messages and remove timestamp
		hashSet.Each(func(v interface{}) bool {
			delete(w.envelopes, v.(common.Hash))
			delete(w.messages, v.(common.Hash))
			return true
		})
		w.expirations[then].Clear()
	}
}

// envelopes retrieves all the messages currently pooled by the node.
func (w *Whisper) Envelopes() []*Envelope {
	w.poolMu.RLock()
	defer w.poolMu.RUnlock()

	all := make([]*Envelope, 0, len(w.envelopes))
	for _, envelope := range w.envelopes {
		all = append(all, envelope)
	}
	return all
}

// Messages retrieves all the decrypted messages matching a filter id.
func (w *Whisper) Messages(id uint32) []*ReceivedMessage {
	result := make([]*ReceivedMessage, 0)
	w.poolMu.RLock()
	defer w.poolMu.RUnlock()

	if filter := w.filters.Get(id); filter != nil {
		for _, msg := range w.messages {
			if filter.MatchMessage(msg) {
				result = append(result, msg)
			}
		}
	}
	return result
}

func (w *Whisper) addDecryptedMessage(msg *ReceivedMessage) {
	w.poolMu.Lock()
	defer w.poolMu.Unlock()

	w.messages[msg.EnvelopeHash] = msg
}

func ValidatePublicKey(k *ecdsa.PublicKey) bool {
	return k != nil && k.X != nil && k.Y != nil && k.X.Sign() != 0 && k.Y.Sign() != 0
}

func validatePrivateKey(k *ecdsa.PrivateKey) bool {
	if k == nil || k.D == nil || k.D.Sign() == 0 {
		return false
	}
	return ValidatePublicKey(&k.PublicKey)
}

// validateSymmetricKey returns false if the key contains all zeros
func validateSymmetricKey(k []byte) bool {
	return len(k) > 0 && !containsOnlyZeros(k)
}

func containsOnlyZeros(data []byte) bool {
	for _, b := range data {
		if b != 0 {
			return false
		}
	}
	return true
}

func bytesToIntLittleEndian(b []byte) (res uint64) {
	mul := uint64(1)
	for i := 0; i < len(b); i++ {
		res += uint64(b[i]) * mul
		mul *= 256
	}
	return res
}

func BytesToIntBigEndian(b []byte) (res uint64) {
	for i := 0; i < len(b); i++ {
		res *= 256
		res += uint64(b[i])
	}
	return res
}

// DeriveSymmetricKey derives symmetric key material from the key or password.
// pbkdf2 is used for security, in case people use password instead of randomly generated keys.
func deriveKeyMaterial(key []byte, version uint64) (derivedKey []byte, err error) {
	if version == 0 {
		// kdf should run no less than 0.1 seconds on average compute,
		// because it's a once in a session experience
		derivedKey := pbkdf2.Key(key, nil, 65356, aesKeyLength, sha256.New)
		return derivedKey, nil
	} else {
		return nil, unknownVersionError(version)
	}
}
