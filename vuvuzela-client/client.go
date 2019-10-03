package main

import (
	"fmt"
	"os"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/gorilla/websocket"

	. "github.com/numbleroot/vuvuzela"
)

type Client struct {
	sync.Mutex

	EntryServer string
	MyPublicKey *BoxKey

	ws *websocket.Conn

	roundHandlers map[uint32]ConvoHandler
	convoHandler  ConvoHandler

	MetricsPipe *os.File
}

type ConvoHandler interface {
	NextConvoRequest(round uint32) *ConvoRequest
	HandleConvoResponse(response *ConvoResponse)
}

func NewClient(entryServer string, publicKey *BoxKey, metricsPipe *os.File) *Client {

	return &Client{
		EntryServer:   entryServer,
		MyPublicKey:   publicKey,
		roundHandlers: make(map[uint32]ConvoHandler),
		MetricsPipe:   metricsPipe,
	}
}

func (c *Client) SetConvoHandler(convo ConvoHandler) {

	c.Lock()
	c.convoHandler = convo
	c.Unlock()
}

func (c *Client) Connect() error {

	// TODO check if already connected
	if c.convoHandler == nil {
		return fmt.Errorf("no convo handler")
	}

	wsaddr := fmt.Sprintf("%s/ws?publickey=%s", c.EntryServer, c.MyPublicKey.String())
	dialer := &websocket.Dialer{
		HandshakeTimeout: 5 * time.Second,
	}

	tried := 1

	// Reliably connect to entry server.
	ws, _, err := dialer.Dial(wsaddr, nil)
	for (err != nil) && (tried <= 20) {

		fmt.Printf("Connecting to entry server failed %d time(s), will try again: %v\n", tried, err)
		time.Sleep(100 * time.Millisecond)
		tried++

		ws, _, err = dialer.Dial(wsaddr, nil)
	}

	if err != nil {

		// If connecting to the entry server failed
		// too many times, give up.
		fmt.Printf("Connecting to entry server failed %d times, returning with error.\n", tried)

		return err
	}

	c.ws = ws

	return nil
}

func (c *Client) Close() {
	c.ws.Close()
}

func (c *Client) Send(v interface{}) {

	const writeWait = 10 * time.Second

	e, err := Envelop(v)
	if err != nil {

		log.WithFields(log.Fields{
			"bug":  true,
			"call": "Envelop",
		}).Error(err)

		return
	}

	c.Lock()
	c.ws.SetWriteDeadline(time.Now().Add(writeWait))

	err = c.ws.WriteJSON(e)
	if err != nil {

		log.WithFields(log.Fields{
			"call": "WriteJSON",
		}).Debug(err)

		c.Unlock()
		c.Close()

		return
	}

	// Pipe out send time of message
	// to metrics collector.
	sendTime := time.Now().UnixNano()
	fmt.Fprintf(c.MetricsPipe, "send;%d\n", sendTime)

	c.Unlock()
}

func (c *Client) handleResponse(v interface{}, recvTime int64) {

	switch v := v.(type) {
	case *BadRequestError:
		log.Printf("bad request error: %s", v.Error())
	case *AnnounceConvoRound:
		c.Send(c.nextConvoRequest(v.Round))
	case *ConvoResponse:

		// Pipe out receive time of message
		// to metrics collector.
		fmt.Fprintf(c.MetricsPipe, "recv;%d\n", recvTime)

		c.deliverConvoResponse(v)
	}
}

func (c *Client) nextConvoRequest(round uint32) *ConvoRequest {

	c.Lock()
	c.roundHandlers[round] = c.convoHandler
	c.Unlock()

	return c.convoHandler.NextConvoRequest(round)
}

func (c *Client) deliverConvoResponse(r *ConvoResponse) {

	c.Lock()
	convo, ok := c.roundHandlers[r.Round]
	delete(c.roundHandlers, r.Round)
	c.Unlock()

	if !ok {
		log.WithFields(log.Fields{"round": r.Round}).Error("round not found")
		return
	}

	convo.HandleConvoResponse(r)
}
