package server

import (
	"encoding/binary"
	"errors"
	"io"
	"time"

	"bazil.org/bazil/kv"
	"bazil.org/bazil/kv/kvmulti"
	"bazil.org/bazil/peer"
	wirepeer "bazil.org/bazil/peer/wire"
	"bazil.org/bazil/server/wire"
	"bazil.org/bazil/tokens"
	"bazil.org/bazil/util/grpcedtls"
	"github.com/agl/ed25519"
	"github.com/boltdb/bolt"
	"github.com/golang/protobuf/proto"
	"google.golang.org/grpc"
)

var bucketPeer = []byte(tokens.BucketPeer)
var bucketPeerID = []byte(tokens.BucketPeerID)
var bucketPeerAddr = []byte(tokens.BucketPeerAddr)
var bucketPeerStorage = []byte(tokens.BucketPeerStorage)

var (
	ErrPeerNotFound     = errors.New("peer not found")
	ErrNoStorageForPeer = errors.New("no storage offered to peer")
)

func (app *App) findPeer(tx *bolt.Tx, pub *[ed25519.PublicKeySize]byte) (*peer.Peer, error) {
	bucket := tx.Bucket(bucketPeer)
	val := bucket.Get(pub[:])
	if val == nil {
		return nil, ErrPeerNotFound
	}

	var msg wire.Peer
	if err := proto.Unmarshal(val, &msg); err != nil {
		return nil, err
	}
	p := &peer.Peer{
		ID:  peer.ID(msg.Id),
		Pub: pub,
	}
	return p, nil
}

// GetPeer returns a Peer for the given public key.
// If the peer does not exist, returns ErrPeerNotFound.
func (app *App) GetPeer(pub *[ed25519.PublicKeySize]byte) (*peer.Peer, error) {
	var p *peer.Peer
	get := func(tx *bolt.Tx) error {
		var err error
		p, err = app.findPeer(tx, pub)
		return err
	}
	if err := app.DB.View(get); err != nil {
		return nil, err
	}
	return p, nil
}

// MakePeer returns a Peer for the given public key, adding it if
// necessary.
func (app *App) MakePeer(pub *[ed25519.PublicKeySize]byte) (*peer.Peer, error) {
	// try optimistic
	p, err := app.GetPeer(pub)
	if err != ErrPeerNotFound {
		// operational error or success, either is fine here
		return p, err
	}

	getOrMake := func(tx *bolt.Tx) error {
		// try again, in case of race
		var err error
		p, err = app.findPeer(tx, pub)
		if err != ErrPeerNotFound {
			// operational error or success, either is fine here
			return err
		}

		// really not found -> add it; first pick a free id
		bucket := tx.Bucket(bucketPeer)
		var id peer.ID
		idBucket := tx.Bucket(bucketPeerID)
		c := idBucket.Cursor()
		if k, _ := c.Last(); k != nil {
			id = peer.ID(binary.BigEndian.Uint32(k))
		}
		id++
		if id == 0 {
			return errors.New("out of peer IDs")
		}
		var idKey [4]byte
		binary.BigEndian.PutUint32(idKey[:], uint32(id))
		if err := idBucket.Put(idKey[:], pub[:]); err != nil {
			return err
		}
		msg := wire.Peer{
			Id: uint32(id),
		}
		buf, err := proto.Marshal(&msg)
		if err != nil {
			return err
		}
		if err := bucket.Put(pub[:], buf); err != nil {
			return err
		}
		p = &peer.Peer{
			ID:  id,
			Pub: pub,
		}
		return nil
	}
	if err := app.DB.Update(getOrMake); err != nil {
		return nil, err
	}
	return p, nil
}

func (app *App) OpenKVForPeer(pub *[ed25519.PublicKeySize]byte) (kv.KV, error) {
	var msg wire.PeerStorage
	find := func(tx *bolt.Tx) error {
		bucket := tx.Bucket(bucketPeerStorage)
		val := bucket.Get(pub[:])
		if val == nil {
			return ErrNoStorageForPeer
		}
		if err := proto.Unmarshal(val, &msg); err != nil {
			return err
		}
		return nil
	}
	if err := app.DB.View(find); err != nil {
		return nil, err
	}

	var kvstores []kv.KV
	for backend, _ := range msg.Backends {
		s, err := app.openStorage(backend)
		if err != nil {
			return nil, err
		}
		kvstores = append(kvstores, s)
	}

	return kvmulti.New(kvstores...), nil
}

type PeerClient interface {
	wirepeer.PeerClient
	io.Closer
}

type peerClient struct {
	wirepeer.PeerClient
	conn *grpc.ClientConn
}

var _ PeerClient = (*peerClient)(nil)

func (p *peerClient) Close() error {
	return p.conn.Close()
}

func (app *App) DialPeer(pub *peer.PublicKey) (PeerClient, error) {
	lookup := func(network string, addr string) (string, string, *[ed25519.PublicKeySize]byte, error) {
		find := func(tx *bolt.Tx) error {
			bucket := tx.Bucket(bucketPeerAddr)
			val := bucket.Get(pub[:])
			if val == nil {
				return errors.New("no address known for peer")
			}
			addr = string(val)
			return nil
		}
		if err := app.DB.View(find); err != nil {
			return "", "", nil, err
		}
		return network, addr, (*[ed25519.PublicKeySize]byte)(pub), nil
	}

	auth := &grpcedtls.Authenticator{
		Config: app.GetTLSConfig,
		Lookup: lookup,
	}

	// TODO never delay here.
	// https://github.com/grpc/grpc-go/blob/8ce50750fe22e967aa8b1d308b21511844674b57/clientconn.go#L85
	conn, err := grpc.Dial("placeholder.bazil.org.invalid.:443",
		grpc.WithTransportCredentials(auth),
		grpc.WithTimeout(30*time.Second),
	)
	if err != nil {
		return nil, err
	}
	client := wirepeer.NewPeerClient(conn)
	p := &peerClient{
		PeerClient: client,
		conn:       conn,
	}
	return p, nil
}