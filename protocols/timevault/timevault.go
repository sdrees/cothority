package timevault

import (
	"fmt"
	"sync"
	"time"

	"github.com/dedis/cothority/lib/dbg"
	"github.com/dedis/cothority/lib/sda"
	"github.com/dedis/crypto/abstract"
	"github.com/dedis/crypto/config"
	"github.com/dedis/crypto/poly"
	"github.com/dedis/crypto/random"
)

func init() {
	sda.ProtocolRegisterName("TimeVault", NewTimeVault)
}

// Type of shared secret identifiers
type SID string

// Identifiers for TimeVault shared secrets.
const (
	TVSS SID = "TVSS"
)

type TimeVault struct {
	*sda.Node
	keyPair          *config.KeyPair
	pubKeys          []abstract.Point
	info             poly.Threshold
	secrets          map[SID]*Secret // TODO: Could make sense to store pairs of secrets?!
	recoveredSecrets map[SID]*RecoveredSecret
	secretsDone      chan bool
	secretsChan      chan abstract.Secret
}

type Secret struct {
	secret   *poly.SharedSecret // Shared secret
	receiver *poly.Receiver     // Receiver to aggregate deals
	deals    map[int]*poly.Deal // Buffer for deals
	numConfs int                // Number of collected confirmations that shared secrets are ready
	mtx      sync.Mutex         // Mutex to sync access to numConfs
	duration time.Duration      // Duration after which the timer expires
	expired  bool               // Indicator if timer has expired
}

type RecoveredSecret struct {
	PriShares *poly.PriShares
	NumShares int
	mtx       sync.Mutex
}

func NewTimeVault(node *sda.Node) (sda.ProtocolInstance, error) {

	kp := &config.KeyPair{Suite: node.Suite(), Public: node.Public(), Secret: node.Private()}
	n := len(node.List())
	pk := make([]abstract.Point, n)
	for i, tn := range node.List() {
		pk[i] = tn.Entity.Public
	}

	// NOTE: T <= R <= N (for simplicity we use T = R = N; might change later)
	info := poly.Threshold{T: n, R: n, N: n}

	tv := &TimeVault{
		Node:        node,
		keyPair:     kp,
		pubKeys:     pk,
		info:        info,
		secrets:     make(map[SID]*Secret),
		secretsDone: make(chan bool, 1),
		secretsChan: make(chan abstract.Secret, 1),
	}

	// Setup message handlers
	h := []interface{}{
		tv.handleSecInit,
		tv.handleSecConf,
		tv.handleRevInit,
		tv.handleRevShare,
	}
	err := tv.RegisterHandlers(h...)

	return tv, err
}

func (tv *TimeVault) Start() error {
	return nil
}

// Seal encrypts a given message and releases the decryption key after a given time.
func (tv *TimeVault) Seal(msg []byte, duration time.Duration) (SID, abstract.Point, abstract.Point, error) {

	// Generate shared secret for ElGamal encryption
	var err error
	sid := SID(fmt.Sprintf("%s%d", TVSS, tv.Node.Index()))
	err = tv.initSecret(sid, duration)
	if err != nil {
		return "", nil, nil, err
	}
	<-tv.secretsDone

	// Do ElGamal encryption
	kp := config.NewKeyPair(tv.keyPair.Suite)
	m, _ := tv.keyPair.Suite.Point().Pick(msg, random.Stream)
	X := tv.secrets[sid].secret.Pub.SecretCommit()
	c := tv.keyPair.Suite.Point().Add(m, tv.keyPair.Suite.Point().Mul(X, kp.Secret))

	return sid, kp.Public, c, nil
}

// Open tries to decrypt the given ciphertext from the given shared secret ID and emphereal public key
func (tv *TimeVault) Open(sid SID, key abstract.Point, ct abstract.Point) ([]byte, error) {

	secret, ok := tv.secrets[sid]
	if !ok {
		return nil, fmt.Errorf("Error, shared secret does not exist")
	}

	if !secret.expired {
		return nil, fmt.Errorf("Error, secret has not yet expired")
	}

	// Setup list of recovered secrets if necessary
	if tv.recoveredSecrets == nil {
		tv.recoveredSecrets = make(map[SID]*RecoveredSecret)
	}

	rs := &RecoveredSecret{PriShares: &poly.PriShares{}, NumShares: 0}
	rs.PriShares.Empty(tv.keyPair.Suite, tv.info.T, tv.info.N)
	rs.PriShares.SetShare(tv.secrets[sid].secret.Index, *tv.secrets[sid].secret.Share)
	rs.NumShares++
	tv.recoveredSecrets[sid] = rs

	// Start process to reveal shares
	if err := tv.Broadcast(&RevInitMsg{Src: tv.Node.Index(), SID: sid}); err != nil {
		return nil, err
	}
	x := <-tv.secretsChan

	// Do ElGamal decryption
	X := tv.keyPair.Suite.Point().Mul(key, x)
	msg, err := tv.keyPair.Suite.Point().Sub(ct, X).Data()

	return msg, err
}

func (tv *TimeVault) initSecret(sid SID, duration time.Duration) error {

	// Initialise shared secret of given type if necessary
	if _, ok := tv.secrets[sid]; !ok {
		dbg.Lvl2(fmt.Sprintf("Node %d: Initialising %s shared secret", tv.Node.Index(), sid))
		sec := &Secret{
			receiver: poly.NewReceiver(tv.keyPair.Suite, tv.info, tv.keyPair),
			deals:    make(map[int]*poly.Deal),
			numConfs: 0,
			duration: duration,
			expired:  false,
		}
		tv.secrets[sid] = sec
	}

	secret := tv.secrets[sid]

	// Initialise and broadcast our deal if necessary
	if len(secret.deals) == 0 {
		kp := config.NewKeyPair(tv.keyPair.Suite)
		deal := new(poly.Deal).ConstructDeal(kp, tv.keyPair, tv.info.T, tv.info.R, tv.pubKeys)
		dbg.Lvl2(fmt.Sprintf("Node %d: Initialising %v deal", tv.Node.Index(), sid))
		secret.deals[tv.Node.Index()] = deal
		db, _ := deal.MarshalBinary()
		msg := &SecInitMsg{
			Src:      tv.Node.Index(),
			SID:      sid,
			Deal:     db,
			Duration: duration,
		}
		if err := tv.Broadcast(msg); err != nil {
			dbg.Warn("Broadcast failed", err)
		}
	}
	return nil
}

func (tv *TimeVault) finaliseSecret(sid SID) error {
	secret, ok := tv.secrets[sid]
	if !ok {
		return fmt.Errorf("Error, shared secret does not exist")
	}

	dbg.Lvl2(fmt.Sprintf("Node %d: %s deals %d/%d", tv.Node.Index(), sid, len(secret.deals), len(tv.Node.List())))

	if len(secret.deals) == tv.info.T {

		for _, deal := range secret.deals {
			if _, err := secret.receiver.AddDeal(tv.Node.Index(), deal); err != nil {
				return err
			}
		}

		sec, err := secret.receiver.ProduceSharedSecret()
		if err != nil {
			return err
		}
		secret.secret = sec
		secret.mtx.Lock()
		secret.numConfs++
		secret.mtx.Unlock()
		dbg.Lvl2(fmt.Sprintf("Node %d: %v created", tv.Node.Index(), sid))

		// Broadcast that we have finished setting up our shared secret
		msg := &SecConfMsg{
			Src: tv.Node.Index(),
			SID: sid,
		}
		if err := tv.Broadcast(msg); err != nil {
			dbg.Warn("Broadcast failed", err)
		}

		// Start timer for revealing secret
		timer := time.NewTimer(secret.duration)
		go func() {
			<-timer.C
			secret.expired = true
		}()

	}
	return nil
}
