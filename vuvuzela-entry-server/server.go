package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/gorilla/websocket"

	. "github.com/numbleroot/vuvuzela"
	. "github.com/numbleroot/vuvuzela/tools"
	"github.com/numbleroot/vuvuzela/vrpc"
	"vuvuzela.io/concurrency"
)

type server struct {
	connectionsMu sync.Mutex
	connections   map[*connection]bool

	convoMu       sync.Mutex
	convoRound    uint32
	convoRequests []*convoReq

	firstServer *vrpc.Client
	lastServer  *vrpc.Client

	MetricsPipe *os.File
}

type convoReq struct {
	conn  *connection
	onion []byte
}

type connection struct {
	sync.Mutex

	ws        *websocket.Conn
	srv       *server
	publicKey *BoxKey
}

func (srv *server) register(c *connection) {

	srv.connectionsMu.Lock()
	srv.connections[c] = true
	srv.connectionsMu.Unlock()
}

func (srv *server) allConnections() []*connection {

	srv.connectionsMu.Lock()

	conns := make([]*connection, len(srv.connections))

	i := 0
	for c := range srv.connections {
		conns[i] = c
		i++
	}

	srv.connectionsMu.Unlock()

	return conns
}

func broadcast(conns []*connection, v interface{}) {

	concurrency.ParallelFor(len(conns), func(p *concurrency.P) {

		for i, ok := p.Next(); ok; i, ok = p.Next() {
			conns[i].Send(v)
		}
	})
}

func (c *connection) Close() {

	c.ws.Close()

	c.srv.connectionsMu.Lock()
	delete(c.srv.connections, c)
	c.srv.connectionsMu.Unlock()
}

func (c *connection) Send(v interface{}) {

	const writeWait = 10 * time.Second

	e, err := Envelop(v)
	if err != nil {
		log.WithFields(log.Fields{"bug": true, "call": "Envelop"}).Error(err)
		return
	}

	c.Lock()
	c.ws.SetWriteDeadline(time.Now().Add(writeWait))

	err = c.ws.WriteJSON(e)
	if err != nil {
		log.WithFields(log.Fields{"call": "WriteJSON"}).Debug(err)
		c.Unlock()
		c.Close()
		return
	}

	c.Unlock()
}

func (c *connection) readLoop() {

	for {

		var e Envelope

		err := c.ws.ReadJSON(&e)
		if err != nil {
			log.WithFields(log.Fields{"call": "ReadJSON"}).Debug(err)
			c.Close()
			break
		}

		v, err := e.Open()
		if err != nil {
			msg := fmt.Sprintf("error parsing request: %s", err)
			go c.Send(&BadRequestError{Err: msg})
		}

		go c.handleConvoRequest(v.(*ConvoRequest))
	}
}

func (c *connection) handleConvoRequest(r *ConvoRequest) {

	srv := c.srv

	srv.convoMu.Lock()

	currRound := srv.convoRound
	if r.Round != currRound {

		srv.convoMu.Unlock()

		err := fmt.Sprintf("wrong round (currently %d)", currRound)
		go c.Send(&ConvoError{Round: r.Round, Err: err})

		return
	}

	rr := &convoReq{
		conn:  c,
		onion: r.Onion,
	}

	srv.convoRequests = append(srv.convoRequests, rr)

	srv.convoMu.Unlock()
}

func (srv *server) convoRoundLoop() {

	for {

		err := NewConvoRound(srv.firstServer, srv.convoRound)
		if err != nil {
			log.WithFields(log.Fields{"service": "convo", "round": srv.convoRound, "call": "NewConvoRound"}).Error(err)
			time.Sleep(10 * time.Second)
			continue
		}
		log.WithFields(log.Fields{"service": "convo", "round": srv.convoRound}).Info("Broadcast")

		broadcast(srv.allConnections(), &AnnounceConvoRound{srv.convoRound})
		time.Sleep(*receiveWait)

		srv.convoMu.Lock()

		go srv.runConvoRound(srv.convoRound, srv.convoRequests)

		srv.convoRound += 1
		srv.convoRequests = make([]*convoReq, 0, len(srv.convoRequests))

		srv.convoMu.Unlock()
	}
}

func (srv *server) runConvoRound(round uint32, requests []*convoReq) {

	conns := make([]*connection, len(requests))
	onions := make([][]byte, len(requests))

	for i, r := range requests {
		conns[i] = r.conn
		onions[i] = r.onion
	}

	rlog := log.WithFields(log.Fields{"service": "convo", "round": round})
	rlog.WithFields(log.Fields{"call": "RunConvoRound", "onions": len(onions)}).Info()

	replies, err := RunConvoRound(srv.firstServer, round, onions)
	if err != nil {
		rlog.WithFields(log.Fields{"call": "RunConvoRound"}).Error(err)
		broadcast(conns, &ConvoError{Round: round, Err: "server error"})
		return
	}

	rlog.WithFields(log.Fields{"replies": len(replies)}).Info("Success")

	concurrency.ParallelFor(len(replies), func(p *concurrency.P) {

		for i, ok := p.Next(); ok; i, ok = p.Next() {

			reply := &ConvoResponse{
				Round: round,
				Onion: replies[i],
			}

			conns[i].Send(reply)
		}
	})

	if round >= 35 {
		fmt.Fprintf(srv.MetricsPipe, "done\n")
		fmt.Printf("Round %d at coordinator, assuming evaluation done, exiting.\n", round)
		os.Exit(0)
	}
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

func (srv *server) wsHandler(w http.ResponseWriter, r *http.Request) {

	if r.Method != "GET" {
		http.Error(w, "Method not allowed", 405)
		return
	}

	pk, err := KeyFromString(r.URL.Query().Get("publickey"))
	if err != nil {
		http.Error(w, "expecting box key in publickey query parameter", http.StatusBadRequest)
		return
	}

	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Upgrade: %s", err)
		return
	}

	c := &connection{
		ws:        ws,
		srv:       srv,
		publicKey: pk,
	}

	srv.register(c)
	c.readLoop()
}

var addr = flag.String("addr", "0.0.0.0:33001", "http service address")
var pkiPath = flag.String("pki", "confs/pki.conf", "pki file")
var receiveWait = flag.Duration("wait", DefaultReceiveWait, "")

// Evaluation flags.
var metricsPipeFlag = flag.String("metricsPipe", "/tmp/collect", "Specify the named pipe to use for IPC with the collector sidecar.")

func main() {

	flag.Parse()
	log.SetFormatter(&ServerFormatter{})

	pki := ReadPKI(*pkiPath)

	firstServer, err := vrpc.Dial("tcp", pki.FirstServer(), runtime.NumCPU())
	for err != nil {

		log.Printf("vrpc.Dial to first mix failed, will try again: %v", err)
		time.Sleep(150 * time.Millisecond)

		firstServer, err = vrpc.Dial("tcp", pki.FirstServer(), runtime.NumCPU())
	}

	lastServer, err := vrpc.Dial("tcp", pki.LastServer(), 1)
	for err != nil {

		log.Printf("vrpc.Dial to last mix failed, will try again: %v", err)
		time.Sleep(150 * time.Millisecond)

		lastServer, err = vrpc.Dial("tcp", pki.LastServer(), 1)
	}

	srv := &server{
		firstServer:   firstServer,
		lastServer:    lastServer,
		connections:   make(map[*connection]bool),
		convoRound:    0,
		convoRequests: make([]*convoReq, 0, 10000),
	}

	// Open named pipe for sending metrics to collector.
	pipe, err := os.OpenFile(*metricsPipeFlag, os.O_WRONLY, 0600)
	if err != nil {
		fmt.Printf("Unable to open named pipe for sending metrics to collector: %v\n", err)
		os.Exit(1)
	}
	srv.MetricsPipe = pipe

	go srv.convoRoundLoop()

	http.HandleFunc("/ws", srv.wsHandler)

	httpServer := &http.Server{
		Addr: *addr,
	}

	fmt.Printf("Listening on %s for conversation messages.\n", *addr)

	err = httpServer.ListenAndServe()
	if err != nil {
		log.Fatalf("ListenAndServe: %v", err)
	}
}
