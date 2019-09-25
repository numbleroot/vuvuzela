package main

import (
	"flag"
	"fmt"
	"net"
	_ "net/http/pprof"
	"net/rpc"
	"os"
	"runtime"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"

	. "github.com/numbleroot/vuvuzela"
	. "github.com/numbleroot/vuvuzela/tools"
	"github.com/numbleroot/vuvuzela/vrpc"
	vrand "vuvuzela.io/crypto/rand"
)

var confPath = flag.String("conf", "", "config file")
var listenAddr = flag.String("addr", "0.0.0.0:33001", "mix server listen address")
var pkiPath = flag.String("pki", "confs/pki.conf", "pki file")

// Evaluation flags.
var isEvalFlag = flag.Bool("eval", false, "Append this flag to write evaluation metrics out to a collector process.")
var metricsPipeFlag = flag.String("metricsPipe", "/tmp/collect", "Specify the named pipe to use for IPC with the collector sidecar.")

type Conf struct {
	ServerName string
	PublicKey  *BoxKey
	PrivateKey *BoxKey
	ConvoMu    float64
	ConvoB     float64
}

func main() {

	flag.Parse()
	log.SetFormatter(&ServerFormatter{})

	if *confPath == "" {
		log.Fatalf("must specify -conf flag")
	}

	pki := ReadPKI(*pkiPath)
	conf := new(Conf)

	ReadJSONFile(*confPath, conf)
	if conf.ServerName == "" || conf.PublicKey == nil || conf.PrivateKey == nil {
		log.Fatalf("missing required fields: %s", *confPath)
	}

	var err error
	var client *vrpc.Client

	addr := pki.NextServer(conf.ServerName)
	if addr != "" {

		client, err = vrpc.Dial("tcp", addr, runtime.NumCPU())
		for err != nil {

			log.Printf("vrpc.Dial to successor mix failed, will try again: %v", err)
			time.Sleep(150 * time.Millisecond)

			client, err = vrpc.Dial("tcp", addr, runtime.NumCPU())
		}
	}

	var idle sync.Mutex

	convoService := &ConvoService{
		Idle: &idle,

		Laplace: vrand.Laplace{
			Mu: conf.ConvoMu,
			B:  conf.ConvoB,
		},

		PKI:        pki,
		ServerName: conf.ServerName,
		PrivateKey: conf.PrivateKey,

		Client:     client,
		LastServer: client == nil,

		IsEval: *isEvalFlag,
	}

	InitConvoService(convoService)

	if convoService.IsEval {

		// Open named pipe for sending metrics to collector.
		pipe, err := os.OpenFile(*metricsPipeFlag, os.O_WRONLY, 0600)
		if err != nil {
			fmt.Printf("Unable to open named pipe for sending metrics to collector: %v\n", err)
			os.Exit(1)
		}
		convoService.MetricsPipe = pipe
	}

	err = rpc.Register(convoService)
	if err != nil {
		log.Fatalf("rpc.Register: %s", err)
	}

	listen, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		log.Fatal("Listen:", err)
	}

	rpc.Accept(listen)
}
