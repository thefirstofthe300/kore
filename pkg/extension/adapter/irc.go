// Example irc adapter. Expected to be built as a standalone .so.
package main

import (
	"strings"

	irc "github.com/fluffle/goirc/client"
	"github.com/hegemone/kore/pkg/msg"
	log "github.com/sirupsen/logrus"
)

type adapter struct {
	client      *irc.Conn
	ingressChan chan<- msg.IngressInterface
}

type IngressMessage struct {
	RawContent string
	Identity   string
	ChannelID  string
}

func (im IngressMessage) GetAdapterName() string {
	return Adapter.Name()
}

func (im IngressMessage) GetChannelID() string {
	return "#jbot-test"
}

func (im IngressMessage) GetIdentity() string {
	return im.Identity
}

func (im IngressMessage) GetRawMessage() string {
	return im.RawContent
}

func (im IngressMessage) GetParsedMessage() string {
	return im.RawContent[0:len(im.RawContent)]
}

func (a adapter) Name() string {
	return "ex-irc.adapters.kore.nsk.io"
}

func (a *adapter) Listen(ingressCh chan<- msg.IngressInterface) {
	log.Debug("ex-irc.adapters::Listen")
	a.ingressChan = ingressCh

	cfg := irc.NewConfig("kore")
	cfg.Server = "irc.geekshed.net:6667"

	a.client = irc.Client(cfg)

	a.client.HandleFunc(irc.CONNECTED, func(conn *irc.Conn, line *irc.Line) {
		conn.Join("#jbot-test")
	})

	a.client.HandleFunc(irc.PRIVMSG, func(conn *irc.Conn, line *irc.Line) {
		a.ingressChan <- IngressMessage{
			Identity:   line.Nick,
			RawContent: line.Text(),
			ChannelID:  "#jbot-test",
		}
	})

	if err := a.client.Connect(); err != nil {
		log.Printf("Connection error: %s\n", err.Error())
	}
}

func (a *adapter) SendMessage(m msg.Egress) {
	// The irc library we are using truncates messages with \n characters to
	// the first line. As a workaround, split the message on the newline and
	// send each line individually.
	for _, i := range strings.Split(m.Serialize(), "\n") {
		a.client.Privmsg(m.ChannelID, i)
	}
}

// Adapter is the exported plugin symbol picked up by engine
var Adapter adapter
