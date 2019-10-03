package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"
	"golang.org/x/crypto/nacl/box"

	. "github.com/numbleroot/vuvuzela"
	"vuvuzela.io/crypto/onionbox"
)

type Conversation struct {
	sync.RWMutex

	pki           *PKI
	peerName      string
	peerPublicKey *BoxKey
	myPublicKey   *BoxKey
	myPrivateKey  *BoxKey
	gui           *GuiClient

	outQueue      chan []byte
	pendingRounds map[uint32]*pendingRound

	lastPeerResponding bool
	lastLatency        time.Duration
	lastRound          uint32

	MetricsPipe *os.File
}

func (c *Conversation) Init() {

	c.Lock()
	c.outQueue = make(chan []byte, 64)
	c.pendingRounds = make(map[uint32]*pendingRound)
	c.lastPeerResponding = false
	c.Unlock()
}

type pendingRound struct {
	onionSharedKeys []*[32]byte
	sentMessage     [SizeEncryptedMessage]byte
}

type ConvoMessage struct {
	Body interface{}
	// seq/ack numbers can go here
}

type TextMessage struct {
	Message []byte
}

type TimestampMessage struct {
	Timestamp time.Time
}

func (cm *ConvoMessage) Marshal() [SizeMessage]byte {

	var msg [SizeMessage]byte

	switch v := cm.Body.(type) {
	case *TimestampMessage:
		msg[0] = 0
		binary.PutVarint(msg[1:], v.Timestamp.Unix())
	case *TextMessage:
		msg[0] = 1
		copy(msg[1:], v.Message)
	}

	return msg
}

func (cm *ConvoMessage) Unmarshal(msg []byte) error {

	switch msg[0] {
	case 0:
		ts, _ := binary.Varint(msg[1:])
		cm.Body = &TimestampMessage{
			Timestamp: time.Unix(ts, 0),
		}
	case 1:
		cm.Body = &TextMessage{msg[1:]}
	default:
		return fmt.Errorf("unexpected message type: %d", msg[0])
	}

	return nil
}

func (c *Conversation) QueueTextMessage(msg []byte) {
	c.outQueue <- msg
}

func (c *Conversation) NextConvoRequest(round uint32) *ConvoRequest {

	c.Lock()
	c.lastRound = round
	c.Unlock()

	var body interface{}
	var convoID [26]byte
	var seqNo [5]byte

	select {
	case m := <-c.outQueue:
		body = &TextMessage{Message: m}
		copy(convoID[:], m[:26])
		copy(seqNo[:], m[26:31])
	default:
		body = &TimestampMessage{
			Timestamp: time.Now(),
		}
	}

	msg := &ConvoMessage{
		Body: body,
	}

	msgdata := msg.Marshal()

	var encmsg [SizeEncryptedMessage]byte
	ctxt := c.Seal(msgdata[:], round, c.myRole())
	copy(encmsg[:], ctxt)

	exchange := &ConvoExchange{
		DeadDrop:         c.deadDrop(round),
		EncryptedMessage: encmsg,
	}

	onion, sharedKeys := onionbox.Seal(exchange.Marshal(), ForwardNonce(round), c.pki.ServerKeys().Keys())

	pr := &pendingRound{
		onionSharedKeys: sharedKeys,
		sentMessage:     encmsg,
	}

	c.Lock()
	c.pendingRounds[round] = pr
	c.Unlock()

	// Pipe out conversation ID and message
	// sequence number to metrics collector.
	fmt.Fprintf(c.MetricsPipe, "send;%s %s\n", convoID, seqNo)

	return &ConvoRequest{
		Round: round,
		Onion: onion,
	}
}

func (c *Conversation) HandleConvoResponse(r *ConvoResponse) {

	rlog := log.WithFields(log.Fields{"round": r.Round})

	var responding bool
	defer func() {
		c.Lock()
		c.lastPeerResponding = responding
		c.Unlock()
	}()

	c.Lock()
	pr, ok := c.pendingRounds[r.Round]
	delete(c.pendingRounds, r.Round)
	c.Unlock()

	if !ok {
		rlog.Error("round not found")
		return
	}

	encmsg, ok := onionbox.Open(r.Onion, BackwardNonce(r.Round), pr.onionSharedKeys)
	if !ok {
		rlog.Error("decrypting onion failed")
		return
	}

	if bytes.Compare(encmsg, pr.sentMessage[:]) == 0 && !c.Solo() {
		return
	}

	msgdata, ok := c.Open(encmsg, r.Round, c.theirRole())
	if !ok {
		rlog.Error("decrypting peer message failed")
		return
	}

	msg := new(ConvoMessage)
	err := msg.Unmarshal(msgdata)
	if err != nil {
		rlog.Error("unmarshaling peer message failed")
		return
	}

	responding = true

	switch m := msg.Body.(type) {
	case *TextMessage:

		// Pipe out conversation ID and message
		// sequence number to metrics collector.
		fmt.Fprintf(c.MetricsPipe, "recv;%s %s\n", m.Message[:26], m.Message[26:31])

		s := strings.TrimRight(string(m.Message), "\x00")
		fmt.Printf("<%s> %s\n", c.peerName, s)

		msgID, err := strconv.Atoi(string(m.Message[26:31]))
		if err != nil {
			fmt.Printf("Failed to convert message sequence string to number: %v\n", err)
			os.Exit(1)
		}

		if msgID >= 35 {

			fmt.Fprintf(c.MetricsPipe, "done\n")
			fmt.Printf("Number of messages to evaluate received, terminating.\n")

			time.Sleep(1 * time.Second)
			os.Exit(0)
		}

	case *TimestampMessage:
		latency := time.Now().Sub(m.Timestamp)
		c.Lock()
		c.lastLatency = latency
		c.Unlock()
	}
}

type Status struct {
	PeerResponding bool
	Round          uint32
	Latency        float64
}

func (c *Conversation) Status() *Status {

	c.RLock()
	status := &Status{
		PeerResponding: c.lastPeerResponding,
		Round:          c.lastRound,
		Latency:        float64(c.lastLatency) / float64(time.Second),
	}
	c.RUnlock()

	return status
}

func (c *Conversation) Solo() bool {
	return bytes.Compare(c.myPublicKey[:], c.peerPublicKey[:]) == 0
}

// Roles ensure that messages to the peer and messages from
// the peer have distinct nonces.
func (c *Conversation) myRole() byte {

	if bytes.Compare(c.myPublicKey[:], c.peerPublicKey[:]) < 0 {
		return 0
	}

	return 1
}

func (c *Conversation) theirRole() byte {

	if bytes.Compare(c.peerPublicKey[:], c.myPublicKey[:]) < 0 {
		return 0
	}

	return 1
}

func (c *Conversation) Seal(message []byte, round uint32, role byte) []byte {

	var nonce [24]byte
	binary.BigEndian.PutUint32(nonce[:], round)
	nonce[23] = role

	ctxt := box.Seal(nil, message, &nonce, c.peerPublicKey.Key(), c.myPrivateKey.Key())

	return ctxt
}

func (c *Conversation) Open(ctxt []byte, round uint32, role byte) ([]byte, bool) {

	var nonce [24]byte
	binary.BigEndian.PutUint32(nonce[:], round)
	nonce[23] = role

	return box.Open(nil, ctxt, &nonce, c.peerPublicKey.Key(), c.myPrivateKey.Key())
}

func (c *Conversation) deadDrop(round uint32) DeadDrop {

	var id DeadDrop

	if c.Solo() {
		rand.Read(id[:])
	} else {

		var sharedKey [32]byte
		box.Precompute(&sharedKey, c.peerPublicKey.Key(), c.myPrivateKey.Key())

		h := hmac.New(sha256.New, sharedKey[:])
		binary.Write(h, binary.BigEndian, round)
		r := h.Sum(nil)
		copy(id[:], r)
	}

	return id
}
