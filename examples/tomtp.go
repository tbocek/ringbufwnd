package main

import (
	"crypto/ed25519"
	"fmt"
	"io"
	"log"
	"tomtp"
)

var (
	testPrivateSeed1 = [32]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}
	testPrivateSeed2 = [32]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2}
	testPrivateKey1  = ed25519.NewKeyFromSeed(testPrivateSeed1[:])
	testPrivateKey2  = ed25519.NewKeyFromSeed(testPrivateSeed2[:])
	hexPublicKey1    = fmt.Sprintf("0x%x", testPrivateKey1.Public())
	hexPublicKey2    = fmt.Sprintf("0x%x", testPrivateKey2.Public())
)

func handleConnection(stream *tomtp.Stream) {
	defer stream.Close()
	// Echo all incoming data
	io.Copy(stream, stream)
}

func main() {
	go peerOther()

	listener, err := tomtp.Listen(":8881", testPrivateSeed1)
	if err != nil {
		log.Fatalf("Error in listening: %s", err)
	}
	defer listener.Close()

	for {
		stream, err := listener.Accept()
		if err != nil {
			log.Fatalf("Error in accept: %s", err)
		}
		go handleConnection(stream)
	}

}

func peerOther() {
	listener, err := tomtp.Listen(":8882", testPrivateSeed2)
	if err != nil {
		log.Fatalf("Error in listening: %s", err)
	}
	defer listener.Close()

	stream, err := listener.Dial("127.0.0.1:8881", 0, hexPublicKey1)
	if err != nil {
		log.Fatalf("Error in accept: %s", err)
	}

	fmt.Fprintf(stream, "gogogo")
}
