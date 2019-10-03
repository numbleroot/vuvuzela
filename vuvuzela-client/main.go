package main

import (
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"

	. "github.com/numbleroot/vuvuzela"
	. "github.com/numbleroot/vuvuzela/tools"
)

var doInit = flag.Bool("init", false, "create default config file")
var confPath = flag.String("conf", "confs/client.conf", "config file")
var pkiPath = flag.String("pki", "confs/pki.conf", "pki file")
var peer = flag.String("peer", "", "name of peer as registered at PKI")

// Evaluation flags.
var numMsgToRecvFlag = flag.Int("numMsgToRecv", -1, "Specify how many messages the client is supposed to receive before exiting, -1 disables this limit.")
var metricsPipeFlag = flag.String("metricsPipe", "/tmp/collect", "Specify the named pipe to use for IPC with the collector sidecar.")

type Conf struct {
	MyName       string
	MyPublicKey  *BoxKey
	MyPrivateKey *BoxKey
}

type GuiClient struct {
	sync.Mutex

	pki          *PKI
	myName       string
	myPublicKey  *BoxKey
	myPrivateKey *BoxKey

	client        *Client
	selectedConvo *Conversation
	conversations map[string]*Conversation

	NumMsgToRecv int
}

func (gc *GuiClient) switchConversation(peer string, metricsPipe *os.File) {

	var convo *Conversation

	convo, ok := gc.conversations[peer]
	if !ok {

		peerPublicKey, ok := gc.pki.People[peer]
		if !ok {
			fmt.Printf("unknown user: %s", peer)
			return
		}

		convo = &Conversation{
			pki:           gc.pki,
			peerName:      peer,
			peerPublicKey: peerPublicKey,
			myPublicKey:   gc.myPublicKey,
			myPrivateKey:  gc.myPrivateKey,
			gui:           gc,
			MetricsPipe:   metricsPipe,
		}

		convo.Init()
		gc.conversations[peer] = convo
	}

	gc.selectedConvo = convo

	if gc.client != nil {

		convo.Lock()
		convo.lastPeerResponding = false
		convo.lastLatency = 0
		convo.Unlock()

		gc.client.SetConvoHandler(convo)
	}

	fmt.Printf("Now talking to %s\n", peer)
}

func main() {

	flag.Parse()

	pki := ReadPKI(*pkiPath)
	conf := new(Conf)

	ReadJSONFile(*confPath, conf)
	if conf.MyName == "" || conf.MyPublicKey == nil || conf.MyPrivateKey == nil {
		log.Fatalf("missing required fields: %s", *confPath)
	}

	// Open named pipe for sending metrics to collector.
	pipe, err := os.OpenFile(*metricsPipeFlag, os.O_WRONLY, 0600)
	if err != nil {
		fmt.Printf("Unable to open named pipe for sending metrics to collector: %v\n", err)
		os.Exit(1)
	}

	gc := &GuiClient{
		pki:           pki,
		myName:        conf.MyName,
		myPublicKey:   conf.MyPublicKey,
		myPrivateKey:  conf.MyPrivateKey,
		client:        NewClient(pki.EntryServer, conf.MyPublicKey, pipe),
		conversations: make(map[string]*Conversation),
		NumMsgToRecv:  *numMsgToRecvFlag,
	}
	gc.switchConversation(*peer, pipe)

	for msgID := 1; msgID <= 64; msgID++ {

		// Prepare message to send.
		msg := make([]byte, SizeMessage)
		_, err := io.ReadFull(rand.Reader, msg)
		if err != nil {
			fmt.Printf("Failed to prepare random original message %d: %v\n", msgID, err)
			os.Exit(1)
		}

		// Bytes [0, 25] will be conversation ID.
		copy(msg[:], fmt.Sprintf("%s=>%s", gc.myName, *peer))

		// Bytes [26, 30] are the message sequence number.
		copy(msg[26:], fmt.Sprintf("%05d", msgID))

		// Bytes [31, SizeMessage] are the actual message.
		copy(msg[31:], "All human beings are born free and equal in dignity and rights. They are endowed with reason and conscience and should act towards one another in a spirit of brotherhood. Everyone is entitled to all the rights and freedoms set forth in this Declaration, without distinction of any kind, such as race, colour, sex, language, religion, political or other opinion, national or social origin, property, birth or other status. Furthermore, no distinction shall be made on the basis of the political, jurisdictional or international status of the country or territory to which a person belongs, whether it be independent, trust, non-self-governing or under any other limitation of sovereignty.")

		// Append message to outQueue.
		gc.selectedConvo.QueueTextMessage(msg)
	}

	err = gc.client.Connect()
	if err != nil {
		fmt.Printf("Could not connect to coordinator: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Connected to coordinator at %s.\n", gc.pki.EntryServer)

	for {

		var e Envelope

		err := gc.client.ws.ReadJSON(&e)
		if err != nil {
			log.WithFields(log.Fields{"call": "ReadJSON"}).Debug(err)
			gc.client.Close()
			break
		}

		// Save receive time.
		recvTime := time.Now().UnixNano()

		v, err := e.Open()
		if err != nil {
			log.WithFields(log.Fields{"call": "Envelope.Open"}).Error(err)
			continue
		}

		go gc.client.handleResponse(v, recvTime)
	}
}
