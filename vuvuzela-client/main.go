package main

import (
	"flag"
	"fmt"
	"os"
	"sync"

	log "github.com/Sirupsen/logrus"

	. "github.com/numbleroot/vuvuzela"
	. "github.com/numbleroot/vuvuzela/tools"
)

var doInit = flag.Bool("init", false, "create default config file")
var confPath = flag.String("conf", "confs/client.conf", "config file")
var pkiPath = flag.String("pki", "confs/pki.conf", "pki file")
var peer = flag.String("peer", "", "name of peer as registered at PKI")

// Evaluation flags.
var isEvalFlag = flag.Bool("eval", false, "Append this flag to write evaluation metrics out to a collector process.")
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

	IsEval       bool
	NumMsgToRecv int
	MetricsPipe  *os.File
}

func (gc *GuiClient) switchConversation(peer string) {

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

	gc := &GuiClient{
		pki:           pki,
		myName:        conf.MyName,
		myPublicKey:   conf.MyPublicKey,
		myPrivateKey:  conf.MyPrivateKey,
		client:        NewClient(pki.EntryServer, conf.MyPublicKey),
		conversations: make(map[string]*Conversation),
		IsEval:        *isEvalFlag,
		NumMsgToRecv:  *numMsgToRecvFlag,
	}

	if gc.IsEval {

		// Open named pipe for sending metrics to collector.
		pipe, err := os.OpenFile(*metricsPipeFlag, os.O_WRONLY, 0600)
		if err != nil {
			fmt.Printf("Unable to open named pipe for sending metrics to collector: %v\n", err)
			os.Exit(1)
		}
		gc.MetricsPipe = pipe
	}

	gc.switchConversation(*peer)

	gc.selectedConvo.QueueTextMessage([]byte("rofllol"))
	gc.selectedConvo.QueueTextMessage([]byte("xD"))
	gc.selectedConvo.QueueTextMessage([]byte("lmfao"))

	err := gc.client.Connect()
	if err != nil {
		fmt.Printf("Could not connect: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Connected: %s\n", gc.pki.EntryServer)

	for {

		var e Envelope

		err := gc.client.ws.ReadJSON(&e)
		if err != nil {
			log.WithFields(log.Fields{"call": "ReadJSON"}).Debug(err)
			gc.client.Close()
			break
		}

		v, err := e.Open()
		if err != nil {
			log.WithFields(log.Fields{"call": "Envelope.Open"}).Error(err)
			continue
		}

		go gc.client.handleResponse(v)
	}
}
