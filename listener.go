package tomtp

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"log/slog"
	"net"
	"strings"
	"sync"
)

const (
	maxConnections = 1000
	maxBuffer      = 9000 //support large packets
	maxRingBuffer  = 100
	startMtu       = 1400
)

// PubKey is the public key that identifies an peer
type PubKey [32]byte

type Listener struct {
	// this is the port we are listening to
	localConn      *net.UDPConn
	localAddr      *net.UDPAddr
	privKeyId      ed25519.PrivateKey
	privKeyIdCurve *ecdh.PrivateKey
	connMap        map[[8]byte]*Connection // here we store the connection to remote peers, we can have up to
	streamChan     chan *Stream
	errorChan      chan error
	mu             sync.Mutex
}

type Connection struct {
	remoteAddr   *net.UDPAddr       // the remote address
	streams      map[uint32]*Stream // 2^32 connections to a single peer
	mu           sync.Mutex
	listener     *Listener
	pubKeyIdRcv  ed25519.PublicKey
	privKeyEpSnd *ecdh.PrivateKey
	pubKeyEpRcv  *ecdh.PublicKey
	sharedSecret [32]byte
}

func Listen(addr string, seed [32]byte) (*Listener, error) {
	privKeyId := ed25519.NewKeyFromSeed(seed[:])
	return ListenPrivateKey(addr, privKeyId)
}

func ListenPrivateKey(addr string, privKeyId ed25519.PrivateKey) (*Listener, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, err
	}

	err = setDF(conn)
	if err != nil {
		return nil, err
	}

	l := &Listener{
		localConn:      conn,
		localAddr:      udpAddr,
		privKeyId:      privKeyId,
		privKeyIdCurve: ed25519PrivateKeyToCurve25519(privKeyId),
		streamChan:     make(chan *Stream),
		errorChan:      make(chan error),
		connMap:        make(map[[8]byte]*Connection),
		mu:             sync.Mutex{},
	}

	go handleUDP(conn, l)
	return l, nil
}

func handleUDP(conn *net.UDPConn, l *Listener) {
	buffer := make([]byte, maxBuffer)
	for {
		n, remoteAddr, err := conn.ReadFromUDP(buffer)
		if err != nil {
			fmt.Println("Error reading from connection:", err)
			l.errorChan <- err
			return
		}

		connId, err := DecodeConnId(buffer)
		conn2 := l.connMap[connId]

		//no known connection, it is new
		if conn2 == nil {
			m, err := Decode(buffer, n, l.privKeyId, nil, [32]byte{})
			if err != nil {
				fmt.Println("Error reading from connection:", err)
				l.errorChan <- err //TODO: distinguish between error and warning
				continue
			}

			p, err := DecodePayload(bytes.NewBuffer(m.Payload), 0)
			if err != nil {
				fmt.Println("Error reading from connection2:", err)
				l.errorChan <- err
				continue
			}

			s, err := l.getOrCreateStream(p.StreamId, m.PukKeyIdSnd, remoteAddr)
			if err != nil {
				fmt.Println("Error reading from connection:", err)
				l.errorChan <- err
				continue
			}

			err = s.push(m)
			if err != nil {
				l.errorChan <- err
			} else {
				l.streamChan <- s
			}
		} else {
			m, err := Decode(buffer, n, l.privKeyId, conn2.privKeyEpSnd, conn2.sharedSecret)
			if err != nil {
				fmt.Println("Error reading from connection:", err)
				l.errorChan <- err
				continue
			}
			p, err := DecodePayload(bytes.NewBuffer(m.Payload), 0)
			if err != nil {
				fmt.Println("Error reading from connection2:", err)
				l.errorChan <- err
				continue
			}
			s, err := l.getOrCreateStream(p.StreamId, m.PukKeyIdSnd, remoteAddr)
			if err != nil {
				fmt.Println("Error reading from connection:", err)
				l.errorChan <- err
				continue
			}
			err = s.push(m)
		}

	}
}

func (l *Listener) PubKeyId() ed25519.PublicKey {
	return l.privKeyId.Public().(ed25519.PublicKey)
}

func (l *Listener) Close() (error, []error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	var streamErrors []error
	remoteConnError := l.localConn.Close()

	for _, conn := range l.connMap {
		for _, stream := range conn.streams {
			if err := stream.Close(); err != nil {
				streamErrors = append(streamErrors, err)
			}
		}
	}
	l.connMap = make(map[[8]byte]*Connection)
	close(l.errorChan)
	close(l.streamChan)

	return remoteConnError, streamErrors
}

func (l *Listener) new(remoteAddr *net.UDPAddr, pubKeyIdRcv ed25519.PublicKey) (*Connection, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	var arr [8]byte
	copy(arr[0:3], pubKeyIdRcv[0:4])
	pubKey := l.privKeyId.Public().(ed25519.PublicKey)
	copy(arr[3:7], pubKey[0:4])

	if conn, ok := l.connMap[arr]; ok {
		return conn, errors.New("conn already exists")
	}

	//remoteConn, err := l.localConn.WriteToUDP("udp", remoteAddr)
	//if err != nil {
	//	return nil, err
	//}

	privKeyEpSnd, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}

	l.connMap[arr] = &Connection{
		streams:      make(map[uint32]*Stream),
		remoteAddr:   remoteAddr,
		pubKeyIdRcv:  pubKeyIdRcv,
		privKeyEpSnd: privKeyEpSnd,
		mu:           sync.Mutex{},
		listener:     l,
	}
	return l.connMap[arr], nil
}

// based on: https://github.com/quic-go/quic-go/blob/d540f545b0b70217220eb0fbd5278ece436a7a20/sys_conn_df_linux.go#L15
func setDF(conn *net.UDPConn) error {
	rawConn, err := conn.SyscallConn()
	if err != nil {
		return err
	}

	var errDFIPv4, errDFIPv6 error
	if err := rawConn.Control(func(fd uintptr) {
		errDFIPv4 = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_MTU_DISCOVER, unix.IP_PMTUDISC_DO)
		errDFIPv6 = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_MTU_DISCOVER, unix.IPV6_PMTUDISC_DO)
	}); err != nil {
		return err
	}

	switch {
	case errDFIPv4 == nil && errDFIPv6 == nil:
		slog.Debug("Setting DF for IPv4 and IPv6.")
		//TODO: expose this and don't probe for higher MTU when not DF not supported
	case errDFIPv4 == nil && errDFIPv6 != nil:
		slog.Debug("Setting DF for IPv4.")
	case errDFIPv4 != nil && errDFIPv6 == nil:
		slog.Debug("Setting DF for IPv6.")
	case errDFIPv4 != nil && errDFIPv6 != nil:
		slog.Debug("setting DF failed for both IPv4 and IPv6")
	}

	return nil
}

func (l *Listener) getOrCreateStream(streamId uint32, pukKeyIdSnd ed25519.PublicKey, remoteAddr *net.UDPAddr) (*Stream, error) {
	conn, err := l.new(remoteAddr, pukKeyIdSnd)
	if err != nil {
		return nil, err
	}

	if conn.Has(streamId) {
		return conn.Get(streamId)
	}

	return conn.New(streamId)
}

func (l *Listener) Accept() (*Stream, error) {
	select {
	case stream := <-l.streamChan:
		return stream, nil
	case err := <-l.errorChan:
		return nil, err
	}
}

func (l *Listener) Dial(remoteAddrString string, pubKeyId string, streamId uint32) (*Stream, error) {
	remoteAddr, err := net.ResolveUDPAddr("udp", remoteAddrString)
	if err != nil {
		fmt.Println("Error resolving remote address:", err)
		return nil, err
	}

	if strings.HasPrefix(pubKeyId, "0x") {
		pubKeyId = strings.Replace(pubKeyId, "0x", "", 1)
	}

	bytes, err := hex.DecodeString(pubKeyId)
	if err != nil {
		fmt.Println("Error decoding hex string:", err)
		return nil, err
	}
	publicKey := ed25519.PublicKey(bytes)
	return l.DialPublicKey(remoteAddr, publicKey, streamId)
}

func Dial(remoteAddrString string, streamId uint32, pubKeyId string) (*Stream, error) {
	remoteAddr, err := net.ResolveUDPAddr("udp", remoteAddrString)
	if err != nil {
		fmt.Println("Error resolving remote address:", err)
		return nil, err
	}

	bytes, err := hex.DecodeString(pubKeyId)
	if err != nil {
		fmt.Println("Error decoding hex string:", err)
		return nil, err
	}
	publicKey := ed25519.PublicKey(bytes)
	return DialPublicKeyRandomId(remoteAddr, streamId, publicKey)
}

func DialPublicKeyRandomId(remoteAddr *net.UDPAddr, streamId uint32, pukKeyIdSnd ed25519.PublicKey) (*Stream, error) {
	seed, err := generateRandom32()
	if err != nil {
		fmt.Println("Error decoding hex string:", err)
		return nil, err
	}

	//create listener on random port
	l, err := Listen(":0", seed)
	if err != nil {
		fmt.Println("Error decoding hex string:", err)
		return nil, err
	}
	return l.DialPublicKey(remoteAddr, pukKeyIdSnd, streamId)
}

func DialPublicKeyWithId(remoteAddr *net.UDPAddr, pukKeyIdSnd ed25519.PublicKey, privKeyId ed25519.PrivateKey, streamId uint32) (*Stream, error) {
	//create listener on random port
	l, err := ListenPrivateKey(":0", privKeyId)
	if err != nil {
		fmt.Println("Error decoding hex string:", err)
		return nil, err
	}
	return l.DialPublicKey(remoteAddr, pukKeyIdSnd, streamId)
}

func (l *Listener) DialPublicKey(remoteAddr *net.UDPAddr, pukKeyIdSnd ed25519.PublicKey, streamId uint32) (*Stream, error) {
	c, err := l.new(remoteAddr, pukKeyIdSnd)
	if err != nil {
		fmt.Println("Error decoding hex string:", err)
		return nil, err
	}
	return c.New(streamId)
}
